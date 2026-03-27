# TODO

Deferred from v0.1.0 CTO review (2026-03-27).

## P2 — Short term

- [ ] **Extract help-syntax from `commands/template.go`** — the file is 719 lines because ~600 lines are embedded template documentation. Move to `internal/templatehelp/syntax.go` or an embedded `.txt` file. Target: `commands/template.go` under 150 lines.

- [ ] **Add command-level tests for `commands/` package** — currently zero coverage on the user-facing layer. Start with stateless commands (`auth status`, `config set/get`, `version`), then introduce a thin API interface to fake HTTP in tests for the polling-based commands (`template preview`, `playbook trigger`).

- [ ] **Split `internal/api/client.go`** (533 lines) — separate auth token management (validate, refresh, get) from HTTP method helpers. Not urgent until the file needs to change.

## P3 — Before public release

- [ ] **Multi-version CI** — README says "Requires Go 1.21+" but CI only tests Go 1.25. Either add a matrix (`[1.21, 1.25]`) or update the README minimum to Go 1.25.
