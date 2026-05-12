# Task: Taufinity CLI self-update

**Created:** 2026-05-12
**Status:** Planning
**Branch:** `feat/self-update`

## Context

Today `taufinity version` reports `dev / commit: unknown / built: unknown` for installs done via `make install` without explicit ldflags, and there's no way to tell whether the installed binary is behind `main`. Robin runs the CLI daily across customer work — silently drifting versions cause "works on my machine" surprises and force a manual `cd /Users/robin/Documents/code/taufinity-cli && git pull && make install` ritual.

Goal: make the CLI know its own version and tell the user when it's outdated, with a one-command path to upgrade.

## Non-goals

- Auto-applying updates without user consent (footgun: a `git pull` mid-script breaks reproducibility, can pull breaking changes during a customer demo).
- Pre-built binary releases via GitHub releases (no release pipeline today; adding one is out of scope).
- Cross-platform installer (Homebrew tap, scoop, etc.) — Robin and the small team all have Go toolchains.

## Design decisions

### 1. Update mechanism: `go install ...@latest`

```
go install github.com/taufinity/cli/cmd/taufinity@latest
```

**Why:** No local clone required, works from any cwd, single command, uses the user's existing Go toolchain. The repo is public-readable via SSH for the team; `go install` uses HTTPS by default and the repo is now public (`git@github.com:taufinity/cli.git` — confirm before merging).

**Alternative considered:** `cd <clone> && git pull && make install`. Rejected: requires the clone to exist at a known path; fragile across machines; nothing wins us by going this route.

**Caveat:** `go install` against a public module fetches via `proxy.golang.org` which can lag the repo by minutes. Acceptable — staleness is the problem we're solving, not real-time sync.

### 2. Version detection: `debug.ReadBuildInfo()` fallback

Today `Version=dev` is the default in [commands/version.go](../../commands/version.go) when ldflags aren't passed. We add a fallback that reads `runtime/debug.BuildInfo` to extract `vcs.revision`, `vcs.time`, and `vcs.modified` — Go 1.18+ embeds these automatically.

**Resolution order for the displayed version:**
1. ldflag-injected `Version` (release builds via `make install`)
2. Module version from `BuildInfo.Main.Version` (when installed via `go install ...@v1.2.3`)
3. `vcs.revision` short SHA from `BuildInfo.Settings` (when installed via `go install ...@latest` or built from source)
4. Literal string `dev` (final fallback)

Same fallback chain for commit SHA so `taufinity version` is never "unknown" again.

### 3. Staleness check: GitHub commits API, 24h cache, fire-and-forget

On every CLI invocation:

1. Read cache at `~/.config/taufinity/update-check.json`. If `checked_at < 24h ago`, use cached `latest_sha`; skip network.
2. Else, fire a background goroutine that:
   - Calls `GET https://api.github.com/repos/taufinity/cli/commits/main` with a 2s timeout
   - Parses `sha` from response
   - Writes `{checked_at, latest_sha}` to cache
   - **Never blocks the main command.** If it doesn't finish before the command exits, that's fine — next invocation picks up the result.
3. At command exit (via `cobra.OnFinalize` or a deferred check in `Execute()`), compare `current_sha` (from build info) vs `latest_sha` (from cache). If they differ, print to stderr:
   ```
   A newer taufinity is available (abc1234 → def5678). Run: taufinity update
   ```

**Why a background goroutine:** the check must not add latency to short-lived commands like `taufinity auth status`. A 2s timeout on a goroutine means a slow GitHub call delays nothing user-visible — at worst the cache is written after the process would have exited (deferred goroutine, parent exits, GC cleans up). That's fine; next run gets it.

**Why GitHub API not git ls-remote:** no git binary dependency, no SSH key dance, JSON parse is trivial. Unauthenticated rate limit is 60/hour per IP — with 24h caching that's two orders of magnitude of headroom.

### 4. Opt-out

- Env var: `TAUFINITY_NO_UPDATE_CHECK=1` skips the check entirely (for CI, scripts).
- Config: `taufinity config set update_check false` for permanent opt-out.
- Quiet mode: when `--quiet` / `TAUFINITY_QUIET=1` is set, suppress the stderr warning (but still write cache).

### 5. MCP stdio mode: suppress warnings

When running as `taufinity mcp stdio`, the process talks JSON-RPC over stdout to an MCP client. Stderr is usually fine for chatter but to be safe we suppress the update warning in this mode entirely. Detect via the cobra command being executed (`cmd.Name() == "stdio"` under `mcp`).

### 6. `taufinity update` command

```
taufinity update          # run go install ...@latest, print before/after version
taufinity update --check  # report only, no install
```

Implementation: `exec.Command("go", "install", "github.com/taufinity/cli/cmd/taufinity@latest")`, stream stdout/stderr through, then re-exec `taufinity version` to confirm. Requires `go` on PATH — error message points to https://go.dev/dl/ if missing.

## Plan

1. [ ] Add `internal/buildinfo/` package — single source of truth for version/commit, with `BuildInfo()` returning resolved values using the fallback chain.
2. [ ] Refactor `commands/version.go` to call `buildinfo.BuildInfo()` instead of reading package globals directly. Keep ldflag-set globals as the highest-priority source.
3. [ ] Add `internal/updatecheck/` package:
   - `Check(ctx) (latestSHA string, err error)` — calls GitHub API
   - `LoadCache() / SaveCache()` — reads/writes `~/.config/taufinity/update-check.json`
   - `MaybeWarn(out io.Writer, currentSHA string)` — prints stderr warning if outdated, respecting quiet/MCP/opt-out
4. [ ] Wire startup background check into `PersistentPreRunE` in `commands/root.go` — fire-and-forget goroutine with 2s timeout context.
5. [ ] Wire exit warning into `cobra.OnFinalize` or `Execute()` — call `MaybeWarn()` after command runs.
6. [ ] Add `update_check` field to `UserConfig` (default `nil` = enabled, explicit `false` = disabled). Update `config.Set/Get/List/Unset` to handle the new key.
7. [ ] Add `commands/update.go`: `taufinity update` (runs `go install`) and `taufinity update --check` (report only).
8. [ ] MCP stdio suppression: in `commands/mcp_stdio.go` (or root, depending on cleanest hook point), disable update-check side effects when the stdio command runs.
9. [ ] Tests:
   - `internal/buildinfo`: resolution-order table test with mocked BuildInfo
   - `internal/updatecheck`: cache read/write, MaybeWarn behavior under each opt-out path, 24h cache validity
   - `commands/update`: dry-run path (--check), missing-go detection (mock exec.LookPath)
10. [ ] Docs: append a "Self-update" section to README.md with: how the check works, opt-out instructions, manual update command.

## Failure routing

| Phase | On failure → Route to |
|---|---|
| Step 1–2 (buildinfo) | Fix in place — pure refactor, no integration risk |
| Step 3–4 (updatecheck) | Same step — likely a goroutine/timeout bug |
| Step 5 (exit hook) | → Step 4 if MaybeWarn reads stale data, otherwise same step |
| Step 7 (update cmd) | Same step — exec.Command edge cases (Go not on PATH, GOBIN unset) |
| Step 9 (tests) | → relevant earlier step (test reveals real bug) |
| Push / merge | **STOP — Robin decides whether to merge** |

## Verification commands

- `go build ./...` — compiles
- `go test ./...` — unit tests pass
- `go vet ./...` — no issues
- `make install` — installs to `~/bin/taufinity`
- `~/bin/taufinity version` — shows real SHA from BuildInfo (not "unknown")
- `~/bin/taufinity update --check` — reports current vs latest
- `TAUFINITY_NO_UPDATE_CHECK=1 ~/bin/taufinity version` — no warning printed
- `rm -f ~/.config/taufinity/update-check.json && ~/bin/taufinity version` — populates cache file
- `~/bin/taufinity mcp stdio < /dev/null` — no warning on stderr (manual eyeball)

## Files to modify / add

- `internal/buildinfo/buildinfo.go` (new)
- `internal/buildinfo/buildinfo_test.go` (new)
- `internal/updatecheck/check.go` (new)
- `internal/updatecheck/cache.go` (new)
- `internal/updatecheck/check_test.go` (new)
- `commands/version.go` (refactor to use buildinfo)
- `commands/root.go` (wire startup goroutine + exit warning)
- `commands/update.go` (new)
- `commands/update_test.go` (new)
- `commands/mcp_stdio.go` (suppression flag, if cleanest there)
- `internal/config/user.go` (add `UpdateCheck *bool` field, plumb through Set/Get/List/Unset)
- `internal/config/user_test.go` (add cases)
- `README.md` (Self-update section)

## Notes

### 2026-05-12 — Decision: GitHub API over `git ls-remote`

Considered shelling out to `git ls-remote git@github.com:taufinity/cli.git main`. Rejected because (a) requires git binary, (b) requires SSH key for private fetch even though metadata is technically public, (c) no graceful timeout. The HTTPS API is simpler and Robin's machines all have working network egress.

### 2026-05-12 — Decision: stderr warning, not blocking prompt

The warning is informational — the user can ignore it for the entire session if they want. A blocking prompt ("update now? [y/N]") would break scripts and feel pushy. One stderr line per stale day is the right dosage.
