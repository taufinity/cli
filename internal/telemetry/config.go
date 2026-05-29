package telemetry

// SentryDSN for the CLI error-tracking project.
// The DSN is intentionally public — it is safe to embed in client binaries.
// See: https://docs.sentry.io/concepts/key-terms/dsn-explainer/#dsn-is-open
const SentryDSN = "https://fbd21a6ec48a39098e00fdeebf08a57e@o4511035617116160.ingest.de.sentry.io/4511473423941712"

// TelemetryKey is the shared secret for X-Telemetry-Key on the Studio beacon.
// Write-only endpoint + rate limiting makes this acceptable for a public binary.
// Set via -X github.com/taufinity/cli/internal/telemetry.TelemetryKey=... in CI builds.
// When empty (go install by users without ldflags), the beacon is silently skipped.
var TelemetryKey = ""

// StudioURL is the base URL for the Studio beacon endpoint.
var StudioURL = "https://studio.taufinity.io"
