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
	_ = doBeacon(e, 3*time.Second)
}

// sendBeaconSync sends a beacon synchronously and returns any error.
// Used by ReportSync so diagnostic commands get confirmation.
func sendBeaconSync(e Event, timeout time.Duration) error {
	if TelemetryKey == "" || globalDeviceID == "" {
		return nil
	}
	return doBeacon(e, timeout)
}

// doBeacon is the shared implementation for both async and sync beacon sending.
func doBeacon(e Event, timeout time.Duration) error {
	if TelemetryKey == "" || globalDeviceID == "" {
		return nil
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
		return fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/cli/telemetry", StudioURL), bytes.NewReader(body))
	if err != nil {
		slog.Debug("telemetry: create beacon request", "err", err)
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Telemetry-Key", TelemetryKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("telemetry: beacon request failed", "err", err)
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Debug("telemetry: beacon rejected", "status", resp.StatusCode)
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}
