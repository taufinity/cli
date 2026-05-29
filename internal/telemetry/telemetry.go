// Package telemetry provides fire-and-forget error reporting for the CLI.
// It routes events to two sinks: Sentry (crash grouping) and the Studio
// beacon (org-aware error history). Neither sink blocks the caller.
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

// Flush waits for pending Sentry events to drain (call deferred from main).
func Flush() {
	flushSentry()
}
