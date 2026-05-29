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
