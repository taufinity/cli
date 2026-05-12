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
- **Auto-fixing the case where `go install` writes the new binary to a different directory than the currently-running binary** (e.g. installed via `make install` to `~/bin`, then `go install` writes to `$(go env GOPATH)/bin`). We warn; the user fixes their PATH or re-runs `make install`.

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
2. Module version from `BuildInfo.Main.Version` — **but ignore if it equals `(devel)`**, which is what Go reports for non-module builds. Note: `go install ...@latest` against an untagged repo produces a pseudo-version like `v0.0.0-20260512143000-abc1234`; we render that as `abc1234 (2026-05-12)` for readability rather than printing the raw pseudo-version.
3. `vcs.revision` short SHA from `BuildInfo.Settings` (when installed via `go install ...@latest` from a tagged build, or `go build` from source)
4. Literal string `dev` (final fallback)

**Dirty-tree handling (step 2a):** if `BuildInfo.Settings` contains `vcs.modified=true`, append `+dirty` to whatever version string is rendered (e.g. `abc1234+dirty`) AND **disable the staleness check entirely** — `vcs.revision` is the parent SHA of a working tree with uncommitted edits, so comparing it to `origin/main` produces meaningless results. Local dev should be silent.

Same fallback chain for commit SHA so `taufinity version` is never "unknown" again. `vcs.time` populates the `built:` line.

**Out of scope:** `go run` (no installed binary to update) and `go build -trimpath -buildvcs=false` (deliberately strips BuildInfo) both fall through to `dev` — acceptable, both are explicit opt-outs.

### 3. Staleness check: GitHub commits API, 24h cache, bounded-wait background goroutine

On every CLI invocation (unless dirty tree or opt-out):

1. Read cache at `~/.config/taufinity/update-check.json`. If `checked_at < 24h ago`, use cached `latest_sha`; skip network. If the file is missing OR fails to parse, treat as missing and refresh — never error out of the parent command because of a corrupt cache.
2. Else, kick off a background goroutine that:
   - Calls `GET https://api.github.com/repos/taufinity/cli/commits/main` with a 2s context deadline.
   - On 200: parses `sha`, writes cache with `{checked_at: now, latest_sha: sha}`.
   - On 403 / 429 / 5xx / network error: writes cache with `{checked_at: now, latest_sha: ""}` — backs off for the full 24h instead of retrying every invocation. Errors are logged only under `--debug`.
   - **Atomic write:** write to `update-check.json.tmp` then `os.Rename` onto the real path. Same-filesystem rename is atomic, so a mid-write process exit leaves either the old cache or the new — never a torn file.
3. **Bounded wait at exit:** the goroutine is registered with a `sync.WaitGroup`; `Execute()` waits up to **100ms** for it before returning. Short-lived commands (`auth status`, `version`) won't perceive the delay; slow networks lose at most 100ms, but the goroutine still completes in the background of the next shell prompt. This replaces the original "fire-and-forget" — that design risks killing the goroutine mid-write.
4. **Exit warning:** a `defer maybeWarn()` placed inside `Execute()` (root.go) immediately after `rootCmd.Execute()` returns. Compares `current_sha` (from build info) vs `latest_sha` (from cache). If they differ AND we're not opted out AND not in a suppressed-annotation command, print to stderr:
   ```
   A newer taufinity is available (abc1234 → def5678). Run: taufinity update
   ```
   Using a `defer` in `Execute()` (rather than `cobra.OnFinalize`) ensures it fires on RunE errors and on cobra's own usage-error paths. It does NOT fire on `os.Exit()` or panic — acceptable, those are fatal exits where the user has bigger problems.

**Why GitHub API not git ls-remote:** no git binary dependency, no SSH key dance, JSON parse is trivial. Unauthenticated rate limit is 60/hour per IP — with 24h caching that's two orders of magnitude of headroom.

**Concurrency:** two CLI invocations in parallel both writing the cache is handled by the atomic rename — last writer wins, the contents are identical-ish (same `latest_sha` from the same API), no lockfile needed.

### 4. Opt-out

- Env var: `TAUFINITY_NO_UPDATE_CHECK=1` skips the check entirely (for CI, scripts).
- Config: `taufinity config set update_check false` for permanent opt-out.
- Quiet mode: when `--quiet` / `TAUFINITY_QUIET=1` is set, suppress the stderr warning (but still write cache).

### 5. MCP stdio mode: suppress warnings via cobra annotation

When running as `taufinity mcp stdio`, the process talks JSON-RPC over stdout to an MCP client. Stderr is usually fine for chatter but to be safe we suppress the update warning in this mode entirely.

**Mechanism:** set `Annotations: map[string]string{"suppress-update-warning": "true"}` on the stdio cobra command (and any future command that wants the same treatment). In `maybeWarn()`, walk the resolved command tree (`rootCmd.Find(os.Args[1:])`) and skip the warning if any command in the chain carries that annotation. The annotation pattern is extensible — no need to hard-code command names — and avoids the brittleness of `cmd.Name() == "stdio"` (which could collide with a future unrelated `stdio` subcommand elsewhere).

Same annotation also gates whether the **background goroutine** runs at all — MCP stdio servers are long-running and shouldn't make periodic network calls the user didn't ask for.

### 6. `taufinity update` command

```
taufinity update          # run go install ...@latest, print before/after version
taufinity update --check  # report only, no install
```

**Implementation order:**

1. **Pre-flight: is `go` installed?** `exec.LookPath("go")` — if missing, exit with a clear message pointing to https://go.dev/dl/. Do not attempt the install.
2. **Run `go install`:** `exec.Command("go", "install", "github.com/taufinity/cli/cmd/taufinity@latest")` with stdout/stderr streamed through so the user sees module-download progress.
3. **Post-install binary-path check** *(this is the biggest UX trap):* after install, resolve where Go actually wrote the new binary:
   - Read `go env GOBIN` — if non-empty, that's the target.
   - Else read `go env GOPATH` and use `$GOPATH/bin`.
   - Compare to `os.Executable()` (the path of the currently-running CLI).
   - If they differ, print:
     ```
     Installed new taufinity to: /Users/x/go/bin/taufinity
     But you're running from:     /Users/x/bin/taufinity
     The next time you run `taufinity`, you'll still get the old version.
     Fix: either add /Users/x/go/bin to your PATH ahead of /Users/x/bin,
          or run `make install` from a clone of github.com/taufinity/cli.
     ```
   - If they match, print `Updated. Run \`taufinity version\` to confirm.` — we deliberately do NOT auto-exec the new binary (different process, would surprise the user).
4. **`--check` flag:** skip steps 1–3, just call the same `updatecheck.Check()` used by the background goroutine and print `current` vs `latest` to stdout. Exit code 0 if up to date, 1 if behind, 2 on network error — script-friendly.

## Plan

1. [ ] Add `internal/buildinfo/` package — single source of truth for version/commit, with `BuildInfo()` returning resolved values using the fallback chain (including `+dirty` suffix and `IsDirty()` accessor).
2. [ ] Refactor `commands/version.go` to call `buildinfo.BuildInfo()` instead of reading package globals directly. Keep ldflag-set globals as the highest-priority source.
3. [ ] Add `internal/updatecheck/` package:
   - `Check(ctx, httpClient) (latestSHA string, err error)` — calls GitHub API; `httpClient` injectable for tests.
   - `LoadCache() (Cache, error) / SaveCache(Cache) error` — atomic write via tmp+rename; missing or unparseable file returns zero-value cache, never an error to callers.
   - `MaybeWarn(out io.Writer, current buildinfo.Info, cache Cache, opts Options) bool` — prints stderr warning if outdated; returns whether it warned (for tests). Honors quiet, opt-out (env + config), dirty tree, and the cobra `suppress-update-warning` annotation.
   - `RunBackground(ctx) *sync.WaitGroup` — fires the check goroutine; caller waits with bounded timeout.
4. [ ] Wire startup background check into `Execute()` in `commands/root.go` — kick off goroutine before `rootCmd.Execute()`, wait up to 100ms after it returns, then call `MaybeWarn`. Skip entirely if dirty tree, opt-out, or suppressed-annotation command.
5. [ ] Place `defer maybeWarn()` inside `Execute()` after `rootCmd.Execute()` — fires on RunE errors and cobra usage errors. (No `cobra.OnFinalize`; that's less predictable on error paths.)
6. [ ] Add `update_check` field to `UserConfig` as `string` (`""` = default-on, `"false"` = disabled) to stay consistent with existing `site` / `api_url` `string` keys. Update `config.Set/Get/List/Unset` to accept the new key.
7. [ ] Add `commands/update.go`: `taufinity update` (pre-flight `go` check, `go install ...@latest`, **binary-path mismatch warning** comparing GOBIN/GOPATH+bin to `os.Executable()`) and `taufinity update --check` (no install; exit code 0/1/2 = up-to-date/behind/error).
8. [ ] MCP stdio suppression: set `Annotations: map[string]string{"suppress-update-warning": "true"}` on the stdio cobra command in `commands/mcp_stdio.go`. Check the annotation in `Execute()` to skip both the background goroutine and the warning.
9. [ ] Tests:
   - `internal/buildinfo`: resolution-order table test (ldflag > Main.Version > vcs.revision > dev), `(devel)` is ignored, `+dirty` rendering, `IsDirty()` accessor.
   - `internal/updatecheck`:
     - cache atomic write (simulate mid-write exit by killing the write goroutine; ensure either old or new content is present, never empty)
     - cache parse error → treated as missing
     - `Check` against `httptest.NewServer` for: 200 with SHA, 403, 5xx, network error → each writes appropriate cache state
     - `MaybeWarn` opt-out matrix: env var, config flag, quiet, dirty tree, suppress-annotation command, in-date cache, out-of-date cache
     - 24h cache validity boundary
   - `commands/update`:
     - missing `go` (mock `exec.LookPath`)
     - `--check` exit codes
     - binary-path mismatch warning fires when `os.Executable()` differs from resolved install dir
10. [ ] Docs: append a "Self-update" section to README.md with: how the check works, opt-out instructions, manual update command, security note ("anyone with commit access to `main` ships to all CLI users until we move to tagged releases").

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
- `~/bin/taufinity update --check` — reports current vs latest, exits 0 or 1
- `~/bin/taufinity update` — full install path; if GOBIN ≠ `~/bin`, prints the binary-path-mismatch warning
- `TAUFINITY_NO_UPDATE_CHECK=1 ~/bin/taufinity version` — no warning printed
- `rm -f ~/.config/taufinity/update-check.json && ~/bin/taufinity version` — populates cache file with atomic write (verify `update-check.json.tmp` doesn't linger)
- `~/bin/taufinity mcp stdio < /dev/null` — no warning on stderr (manual eyeball)
- **Dirty-tree case:** in the cli source dir, `touch internal/buildinfo/dirty.go && go build -o /tmp/td ./cmd/taufinity && /tmp/td version` → shows `+dirty`, no staleness warning printed even when cache says behind.

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

### 2026-05-12 — Plan revision after CTO review

Applied feedback in the same plan file (not a separate doc). Key changes:
- Replaced "fire-and-forget goroutine" with bounded-wait (100ms) + atomic tmp+rename cache write — prevents torn cache from mid-write exit.
- Added `+dirty` rendering and **disabled staleness check on dirty tree** — `vcs.revision` is the parent SHA on a modified tree and the comparison is meaningless.
- Replaced `cobra.OnFinalize` with `defer maybeWarn()` inside `Execute()` — fires on RunE errors too.
- Replaced `cmd.Name() == "stdio"` with cobra `Annotations["suppress-update-warning"]` — extensible, no brittle name matching.
- **Added the binary-path mismatch warning** to `taufinity update` — handles the case where `go install` writes to `$GOPATH/bin` but `os.Executable()` is at `~/bin` (the loudest expected production complaint).
- Changed `UpdateCheck *bool` → `UpdateCheck string` for consistency with existing `site` / `api_url` config keys.
- Added 403/5xx handling: cache `{checked_at: now, latest_sha: ""}` to back off the full 24h instead of retrying.
- Expanded test list to cover all new edge cases (dirty tree, parse error, atomic write, path mismatch, all opt-out paths).
- Added README security note: anyone with commit access to `main` ships to all users until tagged releases are introduced.
