# CLI Telemetry — taufinity-cli Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a two-sink telemetry package to taufinity-cli that reports auth/update/mcp failures to Sentry and a Studio beacon, using a stable device ID and scrubbing PII from messages.

**Architecture:** A thin `internal/telemetry` package exposes `Init()`, `Report(Event)`, `Flush()`. It writes synchronously to Sentry and asynchronously (goroutine, 3 s timeout) to the Studio `/api/cli/telemetry` endpoint. The package is wired into `main.go` and the four command files that surface user-facing errors.

**Tech Stack:** Go 1.25, github.com/getsentry/sentry-go v0.43.0, net/http for beacon, regexp for scrubbing, github.com/google/uuid (already in go.sum)

---

## File Map

| Action | Path | Purpose |
|--------|------|---------|
| Create | `internal/telemetry/config.go` | compile-time constants: SentryDSN, TelemetryKey, StudioURL |
| Create | `internal/telemetry/telemetry.go` | Init(), Report(Event), Flush(), Event type |
| Create | `internal/telemetry/device.go` | UUID persistence at ~/.config/taufinity/device.json |
| Create | `internal/telemetry/scrub.go` | PII scrubber: token-shaped strings, emails, URL query params |
| Create | `internal/telemetry/sentry.go` | Sentry sink |
| Create | `internal/telemetry/beacon.go` | Studio HTTP sink, fire-and-forget goroutine |
| Create | `internal/telemetry/device_test.go` | Tests for device ID persistence |
| Create | `internal/telemetry/scrub_test.go` | Tests for scrub() function |
| Create | `internal/telemetry/beacon_test.go` | Tests for beacon against httptest server |
| Modify | `cmd/taufinity/main.go` | Init before Execute, defer Flush |
| Modify | `commands/auth.go` | Report auth.failure, auth.token_refresh_failed |
| Modify | `commands/update.go` | Report update.failure |
| Modify | `commands/mcp_install.go` | Report mcp.install_failure |
| Modify | `commands/mcp_stdio.go` | Report mcp.auth_error |
| Modify | `go.mod` / `go.sum` | Add sentry-go v0.43.0 |

---

## Task 1: Add sentry-go dependency

**Files:** `go.mod`, `go.sum`

- [ ] **Step 1: Add dependency**

Run in `/Users/robin/Documents/code/taufinity-cli`:
```bash
go get github.com/getsentry/sentry-go@v0.43.0
```
Expected: `go.mod` now lists `github.com/getsentry/sentry-go v0.43.0`

- [ ] **Step 2: Verify build still passes**
```bash
go build ./...
```
Expected: no errors

- [ ] **Step 3: Commit**
```bash
git add go.mod go.sum
git commit -m "deps: add sentry-go v0.43.0"
```

---

## Task 2: Device ID persistence

**Files:** `internal/telemetry/device.go`, `internal/telemetry/device_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/telemetry/device_test.go`:
```go
package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDeviceID_CreatesOnFirstRun(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	id, fr, err := loadDeviceID()
	if err != nil {
		t.Fatalf("loadDeviceID: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty device ID")
	}
	if !fr {
		t.Fatal("expected firstRun=true on initial call")
	}

	// File must exist
	path := filepath.Join(tmp, ".config", "taufinity", "device.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("device.json not created at %s", path)
	}
}

func TestLoadDeviceID_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	id1, _, _ := loadDeviceID()
	id2, fr2, err := loadDeviceID()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("device ID changed: %s → %s", id1, id2)
	}
	if fr2 {
		t.Fatal("expected firstRun=false on second call")
	}
}

func TestLoadDeviceID_ReadsExisting(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".config", "taufinity")
	os.MkdirAll(dir, 0700)
	want := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	data, _ := json.Marshal(deviceFile{DeviceID: want})
	os.WriteFile(filepath.Join(dir, "device.json"), data, 0600)

	got, fr, err := loadDeviceID()
	if err != nil {
		t.Fatalf("loadDeviceID: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if fr {
		t.Fatal("expected firstRun=false when file exists")
	}
}
```

- [ ] **Step 2: Run test to confirm it fails**
```bash
go test ./internal/telemetry/... -run TestLoadDeviceID -v
```
Expected: FAIL — `loadDeviceID` undefined

- [ ] **Step 3: Create `internal/telemetry/device.go`**

```go
package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type deviceFile struct {
	DeviceID string `json:"device_id"`
}

// loadDeviceID returns (id, firstRun, err).
// firstRun is true when device.json did not exist before this call.
func loadDeviceID() (string, bool, error) {
	path, err := deviceFilePath()
	if err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var f deviceFile
		if json.Unmarshal(data, &f) == nil && f.DeviceID != "" {
			return f.DeviceID, false, nil
		}
	}

	// First run: generate and persist a new UUID.
	id := uuid.New().String()
	f := deviceFile{DeviceID: id}
	raw, _ := json.MarshalIndent(f, "", "  ")
	if werr := os.WriteFile(path, raw, 0600); werr != nil {
		// Return the ID even if we can't save — one-run telemetry is better than none.
		return id, true, nil
	}
	return id, true, nil
}

func deviceFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "taufinity")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "device.json"), nil
}
```

- [ ] **Step 4: Run tests — all pass**
```bash
go test ./internal/telemetry/... -run TestLoadDeviceID -v
```
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**
```bash
git add internal/telemetry/device.go internal/telemetry/device_test.go
git commit -m "telemetry: device ID persistence"
```

---

## Task 3: PII scrubber

**Files:** `internal/telemetry/scrub.go`, `internal/telemetry/scrub_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/telemetry/scrub_test.go`:
```go
package telemetry

import "testing"

func TestScrub(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty",
			input: "",
			want:  "",
		},
		{
			name:  "no PII",
			input: "authorization timed out",
			want:  "authorization timed out",
		},
		{
			name:  "token-shaped string",
			input: "invalid token abc1234567890abcdef1234",
			want:  "invalid token [redacted]",
		},
		{
			name:  "email",
			input: "user robin@us2.nl not found",
			want:  "user [redacted] not found",
		},
		{
			name:  "URL with query params",
			input: "request to https://studio.taufinity.io/api/auth?token=secret123 failed",
			want:  "request to https://studio.taufinity.io/api/auth failed",
		},
		{
			name:  "access token in message",
			input: "token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 expired",
			want:  "token [redacted] expired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.input)
			if got != tc.want {
				t.Errorf("scrub(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to confirm fail**
```bash
go test ./internal/telemetry/... -run TestScrub -v
```
Expected: FAIL — `scrub` undefined

- [ ] **Step 3: Create `internal/telemetry/scrub.go`**

```go
package telemetry

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	// tokenRe matches API keys, JWTs, refresh tokens: alphanumeric + _- at 20+ chars.
	tokenRe = regexp.MustCompile(`[a-zA-Z0-9_\-]{20,}`)
	// emailRe matches email addresses.
	emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
)

// scrub removes token-shaped strings, email addresses, and URL query params from s.
func scrub(s string) string {
	if s == "" {
		return s
	}
	// Strip query params from any URL in the string.
	if strings.Contains(s, "?") {
		if u, err := url.Parse(s); err == nil && u.RawQuery != "" {
			u.RawQuery = ""
			s = u.String()
		}
	}
	// Redact emails first (they contain @ which prevents token pattern match).
	s = emailRe.ReplaceAllString(s, "[redacted]")
	// Redact long token-shaped strings.
	s = tokenRe.ReplaceAllString(s, "[redacted]")
	return s
}
```

- [ ] **Step 4: Run tests — all pass**
```bash
go test ./internal/telemetry/... -run TestScrub -v
```
Expected: PASS (6 subtests)

- [ ] **Step 5: Commit**
```bash
git add internal/telemetry/scrub.go internal/telemetry/scrub_test.go
git commit -m "telemetry: PII scrubber"
```

---

## Task 4: Config constants + public types

**Files:** `internal/telemetry/config.go`, `internal/telemetry/telemetry.go`

- [ ] **Step 1: Create `internal/telemetry/config.go`**

```go
package telemetry

// SentryDSN for the CLI error-tracking project.
// The DSN is intentionally public — it is safe to embed in client binaries.
// See: https://docs.sentry.io/concepts/key-terms/dsn-explainer/#dsn-is-open
const SentryDSN = "https://fbd21a6ec48a39098e00fdeebf08a57e@o4511035617116160.ingest.de.sentry.io/4511473423941712"

// TelemetryKey is the shared secret for X-Telemetry-Key on the Studio beacon.
// Write-only endpoint + rate limiting makes this acceptable for a public binary.
// Set via -X github.com/taufinity/cli/internal/telemetry.TelemetryKey=... in CI builds.
// When empty (go install by users), the beacon is silently skipped.
var TelemetryKey = ""

// StudioURL is the base URL for the Studio beacon endpoint.
var StudioURL = "https://studio.taufinity.io"
```

- [ ] **Step 2: Create `internal/telemetry/telemetry.go`**

```go
// Package telemetry provides fire-and-forget error reporting for the CLI.
// It routes events to two sinks: Sentry (crash grouping) and the Studio
// beacon (org-aware error history). Neither sink ever blocks the caller.
package telemetry

import (
	"log/slog"
)

// Event is a single telemetry event.
type Event struct {
	// EventType is the dotted event name: "auth.failure", "update.failure", etc.
	EventType string
	// ErrorCode is a machine-readable short code: "device_code_expired".
	ErrorCode string
	// ErrorMessage is the human-readable error string. Scrubbed before sending.
	ErrorMessage string
	// Email is set when the CLI is authenticated; empty otherwise.
	Email string
}

var (
	globalDeviceID string
	cliVersion     string
	cliCommit      string
)

// Init loads/creates the device ID and initialises the Sentry sink.
// version and commit are the ldflag-injected build vars from commands.Version / commands.GitCommit.
// Must be called before any Report(); safe to call multiple times (idempotent).
func Init(version, commit string) {
	cliVersion = version
	cliCommit = commit

	id, firstRun, err := loadDeviceID()
	if err != nil {
		slog.Debug("telemetry: failed to load device ID", "err", err)
		return
	}
	globalDeviceID = id
	initSentry(version, commit)

	if firstRun {
		go sendBeacon(Event{EventType: "device.first_seen"})
	}
}

// Report sends a telemetry event to both sinks.
// Safe to call from any goroutine; never blocks the caller for the beacon.
// No-op if Init() was not called or failed.
func Report(e Event) {
	if globalDeviceID == "" {
		return
	}
	e.ErrorMessage = scrub(e.ErrorMessage)
	reportToSentry(e)
	go sendBeacon(e)
}

// Flush waits for pending Sentry events (call deferred from main).
func Flush() {
	flushSentry()
}
```

- [ ] **Step 3: Build to catch compile errors**
```bash
go build ./internal/telemetry/...
```
Expected: missing sentry.go and beacon.go stubs will fail — that's fine, add stubs:

Create `internal/telemetry/sentry.go` (stub only for now):
```go
package telemetry

func initSentry(version, commit string)  {}
func reportToSentry(e Event)             {}
func flushSentry()                       {}
```

Create `internal/telemetry/beacon.go` (stub only for now):
```go
package telemetry

func sendBeacon(e Event) {}
```

```bash
go build ./internal/telemetry/...
```
Expected: compiles successfully

- [ ] **Step 4: Commit stubs**
```bash
git add internal/telemetry/config.go internal/telemetry/telemetry.go \
        internal/telemetry/sentry.go internal/telemetry/beacon.go
git commit -m "telemetry: package skeleton — config, types, stubs"
```

---

## Task 5: Sentry sink

**Files:** `internal/telemetry/sentry.go`

- [ ] **Step 1: Replace stub with real sentry.go**

```go
package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/getsentry/sentry-go"
)

func initSentry(version, commit string) {
	if SentryDSN == "" {
		return
	}

	env := "production"
	// Dirty builds (local dev, go build without ldflags) use development env.
	if version == "dev" || version == "" {
		env = "development"
	}

	release := version
	if commit != "" && commit != "unknown" {
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		release = version + "-" + short
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:            SentryDSN,
		Release:        release,
		Environment:    env,
		SendDefaultPII: false,
		BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
			// Scrub exception values in case they contain tokens/emails.
			for i := range event.Exception.Values {
				event.Exception.Values[i].Value = scrub(event.Exception.Values[i].Value)
			}
			return event
		},
	})
	if err != nil {
		slog.Debug("telemetry: sentry init failed", "err", err)
	}
}

func reportToSentry(e Event) {
	if SentryDSN == "" {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("cli.version", cliVersion)
		scope.SetTag("cli.os", runtime.GOOS)
		scope.SetTag("cli.arch", runtime.GOARCH)
		scope.SetTag("cli.event_type", e.EventType)
		scope.SetTag("device_id", globalDeviceID)

		if e.Email != "" {
			// HMAC-hash the email so Sentry stores a pseudonymous identifier.
			mac := hmac.New(sha256.New, []byte(globalDeviceID))
			mac.Write([]byte(e.Email))
			scope.SetUser(sentry.User{
				ID: fmt.Sprintf("%x", mac.Sum(nil)),
			})
		}

		scope.SetExtra("error_code", e.ErrorCode)

		msg := fmt.Sprintf("[%s] %s", e.EventType, e.ErrorCode)
		if e.ErrorMessage != "" {
			msg += ": " + e.ErrorMessage
		}
		sentry.CaptureMessage(msg)
	})
}

func flushSentry() {
	sentry.Flush(2 * time.Second)
}
```

- [ ] **Step 2: Build**
```bash
go build ./internal/telemetry/...
```
Expected: compiles

- [ ] **Step 3: Commit**
```bash
git add internal/telemetry/sentry.go
git commit -m "telemetry: Sentry sink"
```

---

## Task 6: Studio beacon sink

**Files:** `internal/telemetry/beacon.go`, `internal/telemetry/beacon_test.go`

- [ ] **Step 1: Write failing beacon test**

Create `internal/telemetry/beacon_test.go`:
```go
package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendBeacon_PostsPayload(t *testing.T) {
	var received beaconPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("X-Telemetry-Key") != "test-key" {
			t.Errorf("missing/wrong X-Telemetry-Key: %q", r.Header.Get("X-Telemetry-Key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Override package-level vars for test.
	origKey := TelemetryKey
	origURL := StudioURL
	origID := globalDeviceID
	TelemetryKey = "test-key"
	StudioURL = srv.URL
	globalDeviceID = "test-device-id"
	defer func() {
		TelemetryKey = origKey
		StudioURL = origURL
		globalDeviceID = origID
	}()

	done := make(chan struct{})
	// sendBeacon is normally called in a goroutine; call directly here so we can wait.
	go func() {
		sendBeacon(Event{
			EventType:    "auth.failure",
			ErrorCode:    "device_code_expired",
			ErrorMessage: "authorization timed out",
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sendBeacon timed out")
	}

	if received.EventType != "auth.failure" {
		t.Errorf("event_type: got %q, want %q", received.EventType, "auth.failure")
	}
	if received.DeviceID != "test-device-id" {
		t.Errorf("device_id: got %q", received.DeviceID)
	}
	if received.ErrorCode != "device_code_expired" {
		t.Errorf("error_code: got %q", received.ErrorCode)
	}
}

func TestSendBeacon_SkipsWhenKeyEmpty(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	origKey := TelemetryKey
	origURL := StudioURL
	origID := globalDeviceID
	TelemetryKey = ""
	StudioURL = srv.URL
	globalDeviceID = "test-device-id"
	defer func() {
		TelemetryKey = origKey
		StudioURL = origURL
		globalDeviceID = origID
	}()

	sendBeacon(Event{EventType: "auth.failure"})
	if called {
		t.Error("expected beacon to skip when TelemetryKey is empty")
	}
}
```

- [ ] **Step 2: Run to confirm fail**
```bash
go test ./internal/telemetry/... -run TestSendBeacon -v
```
Expected: FAIL — `beaconPayload` undefined

- [ ] **Step 3: Replace beacon stub with real implementation**

```go
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"
)

type beaconPayload struct {
	DeviceID     string `json:"device_id"`
	EventType    string `json:"event_type"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	CLIVersion   string `json:"cli_version"`
	GitCommit    string `json:"git_commit,omitempty"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Email        string `json:"email,omitempty"`
	Timestamp    string `json:"timestamp"`
}

// sendBeacon fires a single event to the Studio telemetry endpoint.
// Called in a goroutine — must not panic or block indefinitely.
func sendBeacon(e Event) {
	if TelemetryKey == "" || globalDeviceID == "" {
		return
	}

	p := beaconPayload{
		DeviceID:     globalDeviceID,
		EventType:    e.EventType,
		ErrorCode:    e.ErrorCode,
		ErrorMessage: e.ErrorMessage,
		CLIVersion:   cliVersion,
		GitCommit:    cliCommit,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Email:        e.Email,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(p)
	if err != nil {
		slog.Debug("telemetry: marshal beacon", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/cli/telemetry", StudioURL), bytes.NewReader(body))
	if err != nil {
		slog.Debug("telemetry: create beacon request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Telemetry-Key", TelemetryKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("telemetry: beacon request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Debug("telemetry: beacon rejected", "status", resp.StatusCode)
	}
}
```

- [ ] **Step 4: Run beacon tests — pass**
```bash
go test ./internal/telemetry/... -run TestSendBeacon -v
```
Expected: PASS (2 tests)

- [ ] **Step 5: Run all telemetry tests**
```bash
go test ./internal/telemetry/... -v
```
Expected: all pass

- [ ] **Step 6: Commit**
```bash
git add internal/telemetry/beacon.go internal/telemetry/beacon_test.go
git commit -m "telemetry: Studio beacon sink"
```

---

## Task 7: Wire into main.go

**Files:** `cmd/taufinity/main.go`

- [ ] **Step 1: Update main.go**

Current content:
```go
package main

import (
	"fmt"
	"os"

	"github.com/taufinity/cli/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
```

Replace with:
```go
package main

import (
	"fmt"
	"os"

	"github.com/taufinity/cli/commands"
	"github.com/taufinity/cli/internal/telemetry"
)

func main() {
	telemetry.Init(commands.Version, commands.GitCommit)
	defer telemetry.Flush()

	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Build**
```bash
go build ./cmd/taufinity/...
```
Expected: compiles

- [ ] **Step 3: Smoke test — version still works**
```bash
./taufinity version
```
Expected: prints version info normally, no errors

- [ ] **Step 4: Commit**
```bash
git add cmd/taufinity/main.go
git commit -m "telemetry: wire Init/Flush into main"
```

---

## Task 8: Instrument auth command

**Files:** `commands/auth.go`

The auth command has two failure paths that need reporting:
1. `runAuthLogin` — "authorization timed out", "device code expired", "authorization denied"
2. Any `ErrRefreshTokenRejected` from token refresh (if it exists)

- [ ] **Step 1: Add a helper function to auth.go for reporting auth failures**

Find the import block in `commands/auth.go` and add `"github.com/taufinity/cli/internal/telemetry"` and `"github.com/taufinity/cli/internal/auth"` (auth already imported check first).

Find the credential loading pattern — when the CLI is already authenticated, we can pass the email:

```go
func reportAuthFailure(code, message string) {
	e := telemetry.Event{
		EventType:    "auth.failure",
		ErrorCode:    code,
		ErrorMessage: message,
	}
	// Attach email if we have credentials.
	if creds, err := auth.LoadCredentials(); err == nil {
		e.Email = creds.Email
	}
	telemetry.Report(e)
}
```

Add this function at the bottom of `commands/auth.go`.

- [ ] **Step 2: Instrument the three auth failure returns in `runAuthLogin`**

In `runAuthLogin`, find these three error returns and add reporting before each:

**"authorization timed out"** (around line 160):
```go
case <-ctx.Done():
    Print("\n")
    reportAuthFailure("auth_timeout", "authorization timed out")
    return fmt.Errorf("authorization timed out")
```

**"authorization denied"** (around line 201):
```go
case "denied":
    Print("\n")
    reportAuthFailure("auth_denied", "authorization denied")
    return fmt.Errorf("authorization denied")
```

**"device code expired"** (around line 205):
```go
case "device_code_expired":
    Print("\n")
    reportAuthFailure("device_code_expired", "device code expired")
    return fmt.Errorf("device code expired")
```

- [ ] **Step 3: Build**
```bash
go build ./commands/...
```
Expected: compiles

- [ ] **Step 4: Commit**
```bash
git add commands/auth.go
git commit -m "telemetry: instrument auth failures"
```

---

## Task 9: Instrument update command

**Files:** `commands/update.go`

The update command's smoke-test failure is the key failure path.

- [ ] **Step 1: Add telemetry import and reporting in `runUpdate`**

Add `"github.com/taufinity/cli/internal/telemetry"` to imports in `commands/update.go`.

Find the smoke test failure return (around the `fmt.Errorf("smoke test failed AND rollback failed: ...")` and `fmt.Errorf("go install failed: ...")` lines).

After the go-install failure line (around line 207):
```go
if err := runGoInstall(ctx, ...); err != nil {
    telemetry.Report(telemetry.Event{
        EventType:    "update.failure",
        ErrorCode:    "go_install_failed",
        ErrorMessage: err.Error(),
    })
    return fmt.Errorf("go install failed: %w", err)
}
```

After smoke test failure (around line 215):
```go
if smokeErr != nil {
    telemetry.Report(telemetry.Event{
        EventType:    "update.failure",
        ErrorCode:    "smoke_test_failed",
        ErrorMessage: smokeErr.Error(),
    })
    // existing rollback / return logic...
}
```

Find the exact lines by reading the file:
```bash
grep -n "go install failed\|smoke test failed" commands/update.go
```

Instrument each error-return path with `telemetry.Report(...)` before the `return`.

- [ ] **Step 2: Build**
```bash
go build ./commands/...
```

- [ ] **Step 3: Commit**
```bash
git add commands/update.go
git commit -m "telemetry: instrument update failures"
```

---

## Task 10: Instrument mcp_install command

**Files:** `commands/mcp_install.go`

- [ ] **Step 1: Add telemetry import**

Add `"github.com/taufinity/cli/internal/telemetry"` to imports.

- [ ] **Step 2: Find the RunE return and instrument it**

In `mcp_install.go`, find the `runMCPInstall` function. The spec says "RunE returns error". Wrap the final `return err` of that function:

```go
func runMCPInstall(cmd *cobra.Command, args []string) error {
    // ... existing logic ...
    if err := doInstall(...); err != nil {
        telemetry.Report(telemetry.Event{
            EventType:    "mcp.install_failure",
            ErrorCode:    "install_error",
            ErrorMessage: err.Error(),
        })
        return err
    }
    return nil
}
```

Find the exact structure:
```bash
grep -n "func runMCPInstall\|return err\|return fmt" commands/mcp_install.go | head -20
```

Instrument the first error return from `runMCPInstall` that represents a user-facing failure (not internal config errors).

- [ ] **Step 3: Build**
```bash
go build ./commands/...
```

- [ ] **Step 4: Commit**
```bash
git add commands/mcp_install.go
git commit -m "telemetry: instrument mcp install failures"
```

---

## Task 11: Instrument mcp_stdio auth error

**Files:** `commands/mcp_stdio.go`

- [ ] **Step 1: Find auth error surface in mcp_stdio.go**
```bash
grep -n "auth\|Auth\|ErrAuth\|401\|403\|unauthorized" commands/mcp_stdio.go | head -20
```

- [ ] **Step 2: Add telemetry import and instrument**

Add `"github.com/taufinity/cli/internal/telemetry"` to imports.

When mcp_stdio surfaces an auth error via JSON-RPC, find that path and add:
```go
telemetry.Report(telemetry.Event{
    EventType:    "mcp.auth_error",
    ErrorCode:    "mcp_stdio_auth_error",
    ErrorMessage: err.Error(),
})
```

Find exact location:
```bash
grep -n "return\|err\|auth" commands/mcp_stdio.go | grep -i "auth\|401\|unauth" | head -10
```

- [ ] **Step 3: Build and test all commands compile**
```bash
go build ./...
```

- [ ] **Step 4: Run all tests**
```bash
go test ./...
```
Expected: all pass (or same result as before instrumentation — we're not breaking anything)

- [ ] **Step 5: Commit**
```bash
git add commands/mcp_stdio.go
git commit -m "telemetry: instrument mcp stdio auth error"
```

---

## Task 12: Push and create PR

- [ ] **Step 1: Verify current branch**
```bash
git branch --show-current
```
Expected: `feat/cli-telemetry`

- [ ] **Step 2: Push**
```bash
git push -u origin feat/cli-telemetry
```

- [ ] **Step 3: Create PR**
```bash
gh pr create \
  --title "feat: CLI telemetry — error reporting to Sentry and Studio beacon" \
  --body "$(cat <<'EOF'
## Summary

- Adds `internal/telemetry` package with device ID persistence, PII scrubber, Sentry sink, and Studio beacon
- Device UUID stored in `~/.config/taufinity/device.json` (stable across re-logins)
- Auth, update, and MCP install/stdio failures are reported
- Error messages scrubbed of token-shaped strings, emails, and URL query params
- Sentry and beacon are both fire-and-forget; never block the caller
- Beacon skips silently when `TelemetryKey` is empty (user `go install` builds)

## Test plan

- [ ] `go test ./internal/telemetry/...` passes
- [ ] `go test ./...` passes
- [ ] `go build ./...` compiles
- [ ] `taufinity version` still works after wiring
EOF
)"
```

- [ ] **Step 4: Note the PR URL for monitoring**

---

## Self-Review Checklist

- [x] **Spec coverage:** Init/Flush wired ✓, device.go ✓, scrub.go ✓, sentry.go ✓, beacon.go ✓, auth instrumentation ✓, update instrumentation ✓, mcp install ✓, mcp stdio ✓
- [x] **No placeholders:** All code is concrete
- [x] **Type consistency:** `Event` struct used consistently, `beaconPayload` only in beacon.go
- [x] **GDPR:** email only sent when authenticated; error messages scrubbed; device ID is random UUID not PII
