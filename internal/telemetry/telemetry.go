// Package telemetry provides fire-and-forget error reporting for the CLI.
// It routes events to two sinks: Sentry (crash grouping) and the Studio
// beacon (org-aware error history). Neither sink blocks the caller.
package telemetry

import (
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/taufinity/cli/internal/terms"
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
// Safe to call multiple times (idempotent after first call).
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
// Safe to call from any goroutine; the beacon is fire-and-forget (goroutine).
// No-op if Init() was not called or failed.
func Report(e Event) {
	if globalDeviceID == "" {
		return
	}
	e.ErrorMessage = scrub(e.ErrorMessage)
	reportToSentry(e)
	go sendBeacon(e)
}

// ReportSync sends a telemetry event to the Studio beacon synchronously and
// returns an error if the beacon call failed. Use for diagnostic commands where
// the caller needs confirmation. The Sentry sink is still fire-and-forget.
// Returns ErrNotConfigured if telemetry is not enabled.
func ReportSync(e Event, timeout time.Duration) error {
	if !Enabled() {
		return ErrNotConfigured
	}
	e.ErrorMessage = scrub(e.ErrorMessage)
	reportToSentry(e)
	return sendBeaconSync(e, timeout)
}

// Enabled reports whether telemetry is active (TelemetryKey set + device ID
// loaded + user has not opted out via TAUFINITY_NO_TELEMETRY=1).
func Enabled() bool {
	return TelemetryKey != "" && globalDeviceID != "" && os.Getenv(terms.EnvNoTelemetry) != "1"
}

// ErrNotConfigured is returned by ReportSync when telemetry is not enabled.
var ErrNotConfigured = errors.New("telemetry not configured (TelemetryKey not set — official builds only)")

// DeviceID returns the anonymous device UUID loaded by Init.
// Returns "" before Init is called or if Init failed.
func DeviceID() string { return globalDeviceID }

// Flush waits for pending Sentry events to drain (call deferred from main).
func Flush() {
	flushSentry()
}
