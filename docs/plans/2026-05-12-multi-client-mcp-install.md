# Task: Multi-client `taufinity mcp install`

**Created:** 2026-05-12
**Status:** Planning
**Branch:** `feat/multi-client-mcp-install`

## Context

`taufinity mcp install` today only writes to Claude Desktop. Robin's team needs the same one-command install for Claude Code, Cursor, VS Code (native MCP, 1.95+), and Antigravity. Mark and Willem use a mix of these on their laptops; manually editing four different JSON files is exactly the friction the install command was meant to remove.

Goal: one CLI flag flips between any supported client, and `--client all` installs to every detected one in a single shot.

## Supported clients

| `--client` value | Config path (per OS) | Top-level key | Notes |
|---|---|---|---|
| `claude-desktop` *(default, today)* | macOS `~/Library/Application Support/Claude/claude_desktop_config.json` / Windows `%APPDATA%\Claude\claude_desktop_config.json` | `mcpServers` | stdio bridge shape |
| `claude-code` | `~/.claude.json` (all OSes) | `mcpServers` | Same shape; read by Claude Code CLI and VS Code extension |
| `cursor` | `~/.cursor/mcp.json` (all OSes) | `mcpServers` | Per-project `<proj>/.cursor/mcp.json` is also valid but global is simpler |
| `vscode` | macOS `~/Library/Application Support/Code/User/mcp.json` / Linux `~/.config/Code/User/mcp.json` / Windows `%APPDATA%\Code\User\mcp.json` | **`servers`** | Different key from everything else |
| `antigravity` | macOS `~/Library/Application Support/Antigravity/mcp.json` / Linux `~/.config/Antigravity/mcp.json` / Windows `%APPDATA%\Antigravity\mcp.json` | `mcpServers` | Format may shift — alpha tool. Best-effort path, document override env var |
| `all` | every detected target above | — | "Detected" = parent dir exists |
| `print` *(today)* | stdout | — | HTTP shape, copy-paste fallback |

## Plan

1. [ ] Extend `internal/desktopconfig/`:
   - Generalise the package: rename functions to allow a custom top-level key (default `mcpServers`).
   - Add `UpsertServerInKey(path, key, name, entry)`, `RemoveServerInKey(path, key, name)`, `HasServerInKey(path, key, name)`.
   - Keep the existing `UpsertServer/RemoveServer/HasServer` as thin wrappers (zero call-site churn).
2. [ ] Add a client registry to `commands/mcp_install.go`:
   - `type mcpClient struct { name, label, defaultLabel, key string; resolvePath func() (string, error); detect func() bool }`
   - One entry per supported client.
3. [ ] Refactor `runMCPInstall` to dispatch on the registry. Keep the `print` short-circuit as-is.
4. [ ] Implement `--client all`:
   - Iterate the registry; call `detect()` on each (= "does the parent dir exist").
   - For each detected target, call the per-client install. Collect successes + skips + errors.
   - Print a tidy summary at the end: `Installed: claude-desktop, cursor / Skipped: vscode (not detected), antigravity (not detected)`.
   - Exit non-zero only if every detected target failed (a partial failure with at least one success is exit 0 with stderr summary).
5. [ ] Update `runMCPUninstall` to support `--client all` the same way.
6. [ ] Update `--client` flag's help text + `mcp install --help` long description.
7. [ ] Tests:
   - Per-client path resolver: confirm correct path on each OS (table-driven, mock `runtime.GOOS` via build tags or pass-through helpers).
   - `--client all` skips when parent dir absent.
   - `--client all` aggregates per-target errors without aborting on the first one.
   - VS Code key is `servers`, not `mcpServers`.
   - `desktopconfig.UpsertServerInKey` with a custom key doesn't clobber sibling top-level keys.
8. [ ] Update README "Commands" table + the `mcp install` documentation section with the new client list.
9. [ ] Cut `v0.3.0` after merge (new client targets warrant a minor bump).

## Verification commands

- `go test ./... -count=1` — all pass
- `make install` — installs into `~/bin/`
- `~/bin/taufinity mcp install --client print` — unchanged behavior
- `~/bin/taufinity mcp install --client cursor --label test-cursor --force` — writes to `~/.cursor/mcp.json`
- `~/bin/taufinity mcp install --client all --label test-all --force` — writes to every detected client + summary
- Verify file contents per target after each install
- `~/bin/taufinity mcp uninstall --client all --label test-all` — removes from every target

## Failure routing

| Phase | On failure → Route to |
|---|---|
| Step 1 (desktopconfig refactor) | Fix in place — pure refactor, mechanical |
| Step 2–3 (registry + dispatch) | Same step — likely a struct layout issue |
| Step 4 (`--client all`) | Same step; lots of branching, write tests first |
| Step 6 (Antigravity path) | If path turns out wrong, ship the path I have + document `TAUFINITY_ANTIGRAVITY_CONFIG` env-var override |
| Tests fail | → relevant earlier step (test reveals real bug) |

## Non-goals

- Auto-detecting which clients the user has installed (vs. our crude "parent dir exists" check) — we don't want to call `ps aux` or grep app bundles. The parent-dir signal is good enough.
- Backing up the existing config before edits — `UpsertServer` is already atomic (tmp + rename) and doesn't delete sibling entries, so the loss surface is limited to that one entry, which the user can re-install if they regret it.
- Per-client transport overrides (e.g. force HTTP for VS Code while stdio everywhere else). All clients get the stdio bridge by default; `--transport http` still applies if forced.
- Per-project `.cursor/mcp.json` and `.vscode/mcp.json` — we always write the global config. A future `--scope project` flag could add this.

## Notes

### 2026-05-12 — Decision: parent-dir presence as "is the client installed"

Considered (a) querying app bundles via `ls /Applications`, (b) parsing `~/Library/Preferences` for the launch counter, (c) running the client binary with `--version`. All too brittle and too platform-specific. The parent dir of the config file is created by the app on first launch — if the dir doesn't exist, the user has never opened that app on this machine, so installing an MCP config for it is wasted work. False positives (dir exists, app uninstalled) just mean we write a config file that's never read — harmless.

### 2026-05-12 — Decision: VS Code uses `servers`, not `mcpServers`

VS Code's native MCP support (1.95+) uses `{ "servers": { "name": {...} } }` at the file root. Different from every other tool in the ecosystem because VS Code added MCP after the spec stabilised and chose to namespace differently. We support it; we don't argue with it.

### 2026-05-12 — Decision: Antigravity path is a best guess

Antigravity is in alpha and has shipped breaking config-layout changes already. The path baked in here is my best read as of 2026-05-12. Mitigation: `TAUFINITY_ANTIGRAVITY_CONFIG=<path>` env var lets users override. If the path turns out wrong in practice, a one-line patch fixes it.
