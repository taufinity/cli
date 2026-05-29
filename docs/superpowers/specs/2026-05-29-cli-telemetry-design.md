# CLI Telemetry & Error Reporting

**Status:** Approved  
**Date:** 2026-05-29

## Problem

When customers struggle with CLI setup (auth failures, MCP installation, update errors) we have zero visibility. We're blind unless they screen-share or we're on-site. This spec describes a lightweight telemetry system that reports errors back to us automatically.

## Architecture

Two-sink model — one reporting surface routes to both destinations without blocking the caller:

```
CLI command fails
      ↓
internal/telemetry.Report(event)
      ├── Sentry sink  → sentry.io (stack traces, error grouping, release tracking)
      └── Studio beacon (goroutine, 3s timeout)
               ↓
          POST /api/cli/telemetry (X-Telemetry-Key auth)
               ↓
          cli_events table
               ↓ (on *.failure / *.error events)
          Slack #cli-errors webhook
               ↓
          /admin/cli-health page
```

## Device Identity

A random UUID is generated on first run and stored at `~/.config/taufinity/device.json`. It never changes across re-logins or token rotations. It is not PII — it is the only way to correlate events from the same install without requiring authentication.

```json
{ "device_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479" }
```

## Event Payload

```json
{
  "device_id":     "f47ac10b-...",
  "event_type":    "auth.failure",
  "error_code":    "device_code_expired",
  "error_message": "authorization timed out",

  "cli_version":   "0.4.1",
  "git_commit":    "a3f8c12",
  "os":            "darwin",
  "arch":          "arm64",
  "go_version":    "go1.25.0",

  "org_id":        42,
  "email":         "x@y.com",

  "timestamp":     "2026-05-29T10:04:00Z"
}
```

`org_id` and `email` are omitted when the CLI is not authenticated.

## Event Types

| Event type | Trigger |
|---|---|
| `auth.failure` | Login timed out, denied, or device code expired |
| `auth.token_refresh_failed` | Refresh token rejected by server (ErrRefreshTokenRejected) |
| `mcp.install_failure` | `mcp install` RunE returns error |
| `mcp.auth_error` | MCP stdio bridge surfaces auth error via JSON-RPC |
| `update.failure` | Smoke test fails or `go install` exits non-zero |
| `device.first_seen` | First run (device.json did not exist) |

No success events, no command analytics — purely failure signals.

## CLI Package Structure

```
internal/telemetry/
  telemetry.go   Init(), Report(Event), Flush(), public types
  device.go      UUID persistence (~/.config/taufinity/device.json)
  sentry.go      Sentry sink — wraps sentry-go SDK
  beacon.go      Studio HTTP sink — fire-and-forget goroutine, 3s timeout
```

`Init()` is called from `cmd/taufinity/main.go` before `cobra.Execute()`.  
`Flush()` is deferred in `main.go` after `Execute()` returns (required for Sentry CLI flush).

### Sentry configuration

- DSN injected via ldflags: `-X github.com/taufinity/cli/internal/telemetry.SentryDSN=https://fbd21...`
- Release: `Version-GitCommit` (matches existing build vars)
- Environment: `production` in release builds, `development` for dirty/local builds (via `buildinfo.Dirty`)
- `SendDefaultPII: false`
- `BeforeSend` hook: strip token-shaped strings (`[a-zA-Z0-9_-]{20,}`) and URLs from `error_message`
- Tags always present: `cli.version`, `cli.os`, `cli.arch`, `device_id`
- Tags when authenticated: `org_id`, user set to HMAC-hashed email
- `sentry.Flush(2 * time.Second)` at exit

### Studio beacon

- Header: `X-Telemetry-Key: <key>` — shared key baked in via ldflags, validated server-side
- Timeout: 3 seconds (never blocks the user)
- When authenticated: attach `org_id` and `email` from stored credentials
- `error_message` scrubbed of token-shaped strings before sending

## Studio API

### Endpoint: `POST /api/cli/telemetry`

- Authentication: `X-Telemetry-Key` header (stored as `sitegen-cli-telemetry-key` in Secret Manager)
- No user session required — works for unauthenticated users
- Rate limiting: 20 events/min per `device_id` (token bucket), 5 req/s per IP (backstop)
- Max payload: 4KB
- Strips PII from `error_message` before persisting
- On insert: if `event_type` ends in `.failure` or `.error` → async Slack webhook

### DB migration

```sql
CREATE TABLE cli_events (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id     TEXT NOT NULL,
  org_id        INTEGER REFERENCES organizations(id),
  email         TEXT,
  cli_version   TEXT NOT NULL,
  git_commit    TEXT,
  os            TEXT,
  arch          TEXT,
  event_type    TEXT NOT NULL,
  error_code    TEXT,
  error_message TEXT,
  metadata      TEXT,
  created_at    DATETIME NOT NULL DEFAULT (CURRENT_TIMESTAMP)
);
CREATE INDEX idx_cli_events_org_id    ON cli_events(org_id);
CREATE INDEX idx_cli_events_type      ON cli_events(event_type);
CREATE INDEX idx_cli_events_created   ON cli_events(created_at);
```

### Slack notification

Format (to `#cli-errors` or configurable webhook):
```
[auth.failure] device f47ac10b | darwin/arm64 | v0.4.1-a3f8c12
org: VoorPositiviteit (42) | robin@us2.nl
error: device_code_expired — authorization timed out
```

Webhook URL: `sitegen-slack-webhook` secret (existing) or a new dedicated `sitegen-cli-slack-webhook`.

### Admin page: `/admin/cli-health`

- Lists last 100 CLI error events, newest first
- Columns: timestamp, org, email, device_id (first 8 chars), os/arch, version, event_type, error
- Simple React table — no pagination needed at current scale
- Accessible only to admin users (existing admin middleware)

## GDPR / Privacy

- Device ID is a random UUID — not PII
- Email is collected only when authenticated (legitimate service data)
- `error_message` is scrubbed before both Sentry and Studio storage:
  - Token-shaped strings (`[a-zA-Z0-9_\-]{20,}`) → `[redacted]`
  - Email addresses → `[redacted]`
  - URLs with query params → strip query string
- Sentry `BeforeSend` hook strips email from the message string; email only in structured user context (HMAC-hashed)
- Data retention: `cli_events` older than 90 days can be purged (add to existing cleanup job)
- This is technical service data for operating the platform — covered under service T&Cs, no opt-in required

## Infrastructure

### New Secret Manager secrets

| Secret | Purpose |
|---|---|
| `sitegen-cli-telemetry-key` | Shared key for `X-Telemetry-Key` header validation |
| `taufinity-cli-sentry-dsn` | Already exists (Sentry project for CLI) |

### Build vars (taufinity-cli Makefile / CI)

```
-X github.com/taufinity/cli/internal/telemetry.SentryDSN=$(SENTRY_DSN)
-X github.com/taufinity/cli/internal/telemetry.TelemetryKey=$(TELEMETRY_KEY)
-X github.com/taufinity/cli/internal/telemetry.StudioURL=https://studio.taufinity.io
```

## Out of scope

- Success event tracking / command analytics
- Per-org device dashboard (v2)
- Update version staleness alerts (v2)
- Opt-in/opt-out UI
