// Package terms manages first-run privacy notice display and acceptance state.
// The pkg installer writes the acceptance flag via postinstall so the binary
// notice is skipped for pkg-installed users; Homebrew/direct-download users
// see it on first invocation.
package terms

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/taufinity/cli/internal/config"
)

// EnvNoTelemetry disables all data collection when set to "1".
const EnvNoTelemetry = "TAUFINITY_NO_TELEMETRY"

const notice = `Taufinity CLI collects anonymous usage data (device ID, OS, version, commands run).
Set TAUFINITY_NO_TELEMETRY=1 to opt out. Details: https://taufinity.io/privacy/cli`

func flagPath() string {
	return filepath.Join(config.Dir(), "privacy_accepted")
}

// IsAccepted reports whether the privacy notice has been shown/accepted.
func IsAccepted() bool {
	_, err := os.Stat(flagPath())
	return err == nil
}

// Accept creates the acceptance flag file.
func Accept() error {
	p := flagPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o600)
}

// ShowOnce prints the privacy notice to w if it hasn't been shown before,
// then marks it as accepted. Silent no-op on subsequent calls and when
// TAUFINITY_NO_TELEMETRY=1 (user already opted out; no need to explain what
// they opted out of). Errors writing the flag are silently ignored so a
// read-only home directory doesn't break normal usage.
func ShowOnce(w io.Writer) {
	if os.Getenv(EnvNoTelemetry) == "1" {
		return
	}
	if IsAccepted() {
		return
	}
	fmt.Fprintln(w, notice)
	fmt.Fprintln(w)
	_ = Accept()
}
