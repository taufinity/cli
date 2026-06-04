# Task: Honor `--org` for `taufinity tokens` commands

**Created:** 2026-06-04
**Status:** Planning
**Branch:** `feat/tokens-org-override`

## Context

`taufinity tokens create` mints a token in the user's "current organization" only. There is no way from the CLI to mint a token in a different org the user is a member of, even though the server already supports `X-Organization-ID` for JWT auth (added in `api/middleware/auth.go:829-832` in ai-site-gen — JWT-authenticated requests can target any org the user has access to).

This forced a workaround today: grab the JWT via `taufinity auth token`, curl the personal-token endpoint directly with `X-Organization-ID: 12`. That works but defeats the point of having a CLI — `taufinity tokens create` should behave like a member would expect: pass `--org 12` and get a token in org 12.

Same gap on `tokens list` and `tokens revoke` — a user with access to multiple orgs cannot manage tokens in another org without changing their "current org" via the web UI.

The plumbing already exists. `api.Client` has `SetOrg(orgID)` and `setOrgHeader()` that add the `X-Organization-ID` header on every authenticated request. The global `--org` flag is defined in [commands/root.go:68](../../commands/root.go) and `GetOrg()` returns the resolved value with the standard flag → env → config precedence. Playbook commands already use this pattern (see [commands/playbook.go:140-144](../../commands/playbook.go)). Tokens commands just never call `SetOrg()`.

## Non-goals

- Adding a separate `--org` flag on the `tokens` subcommand. The global flag already exists; reusing it keeps a single code path.
- Changing the server endpoint. The server already does the right thing — this is purely a CLI plumbing fix.
- Reworking how "current org" is tracked across the rest of the CLI. The org resolution chain in `GetOrg()` is reused as-is.
- Letting unauthenticated users mint tokens. Auth check stays.
- Org-switching command (`taufinity org switch`). That's a separate, larger feature; this PR enables the use case via flag override only.

## Design decisions

### 1. Reuse the global `--org` flag, follow the playbook pattern

The pattern in `commands/playbook.go` is:

```go
client := api.New(GetAPIURL())
client.SetDebug(IsDebug())
if org := GetOrg(); org != "" {
    client.SetOrg(org)
}
```

Apply the same three-line addition to `runTokensCreate`, `runTokensList`, `runTokensRevoke` in [commands/tokens.go](../../commands/tokens.go). No new flags, no new globals, no new resolution logic.

**Why not a tokens-local `--org` flag?** Would duplicate the resolution chain (env, config). Single code path.

### 2. Update the global `--org` flag help text

Today: `"Override organization ID (for playbook commands)"`.
After: `"Override organization ID"`.

The parenthetical is now misleading — `--org` will work for playbook AND tokens commands (and any future command that follows the pattern). Drop the qualifier so users know the flag has broader scope.

### 3. Validate the org value? No.

The server is authoritative. If a user passes `--org 999999` and isn't a member, the server returns the appropriate error (forbidden / not found). Adding client-side validation duplicates server logic and risks drift. Keep it dumb on the CLI side.

### 4. Tests

`commands/tokens.go` has no existing test file. Add `commands/tokens_test.go` covering:
- `runTokensCreate` with `--org` flag sets the `X-Organization-ID` header on the outgoing request
- `runTokensCreate` without `--org` does not set the header
- `runTokensList` with `--org` sets the header
- `runTokensRevoke` with `--org` sets the header

Strategy: spin up an `httptest.Server` in the test, point the CLI at it, set `flagOrg` directly (it's a package-level var in `commands`), invoke the `runTokens*` functions, assert the captured request header.

Reset `flagOrg` and any auth state between subtests so they don't bleed.

## Implementation steps

Each step ends with a verification command. Agent runs verification before moving on.

1. [ ] **Add `SetOrg` call to `runTokensCreate`**
   - Edit [commands/tokens.go](../../commands/tokens.go) — after `client.SetDebug(IsDebug())` add `if org := GetOrg(); org != "" { client.SetOrg(org) }`.
   - Verify: `go build ./...` passes.

2. [ ] **Same for `runTokensList`**
   - Same three-line addition.
   - Verify: `go build ./...` passes.

3. [ ] **Same for `runTokensRevoke`**
   - Same three-line addition.
   - Verify: `go build ./...` passes.

4. [ ] **Update global `--org` flag description in [commands/root.go:68](../../commands/root.go)**
   - Change `"Override organization ID (for playbook commands)"` → `"Override organization ID"`.
   - Verify: `taufinity --help` shows the new text without the parenthetical.

5. [ ] **Add `commands/tokens_test.go`**
   - Tests as described in design decision 4.
   - Verify: `go test ./commands/... -run TestTokens` passes.

6. [ ] **Run full local validation**
   - `go vet ./...`
   - `go test ./...`
   - `go build ./...`
   - All three must pass before merge.

7. [ ] **Manual end-to-end smoke test (against prod)**
   - With `taufinity auth status` showing VoorPositiviteit (my current default org), run:
     ```
     taufinity --api-url https://studio.taufinity.io --org 12 tokens create --name "e2e-org-flag-test" --expires 1d
     ```
   - Use the returned token to call `GET /api/sites` — should list Taufinity Blog (org 12) only, not VoorPositiviteit.
   - Then `taufinity --api-url https://studio.taufinity.io --org 12 tokens list` — should show the new token (and any other Taufinity-org personal tokens).
   - Then `taufinity --api-url https://studio.taufinity.io --org 12 tokens revoke <id>` — clean up.

## Failure routing

| Phase | On failure → Route to |
|---|---|
| Step 1–4 (code edits) | Same step — small enough to fix in place |
| Step 5 (tests) | Step 1–3 if test reveals a wiring bug; same step if test scaffolding issue |
| Step 6 (vet/test/build) | Earlier step pointed to by the error |
| Step 7 (e2e) | **Stop and debug** — likely server behavior surprise; do not paper over |
| CI (after push) | Read failure, route back to step pointed at; never `--no-verify` |

## Rollback

Trivial. Either:
- Revert the tokens-org-override commit on `main`, or
- One-line revert of the four edits (three `SetOrg` calls + help text). No data migration, no server change, no breaking API change.

No customer impact — the previous behavior (no `--org`) remains valid for users who don't pass the flag.

## Open questions

None. The plumbing is in place server-side and in `api.Client`. This is the missing 8-line wiring.
