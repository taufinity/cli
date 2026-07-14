package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type trackerCall struct {
	Method string
	Path   string
	Body   map[string]any
}

func trackerServer(t *testing.T, calls *[]trackerCall, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(b, &parsed)
		mu.Lock()
		*calls = append(*calls, trackerCall{Method: r.Method, Path: r.URL.Path, Body: parsed})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
}

func writeTrackerYAML(t *testing.T, dir, body string) string {
	t.Helper()
	siteDir := filepath.Join(dir, "site-a")
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "tracker.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return siteDir
}

func TestProvisionTrackerHappyPath(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), `
enabled: true
write_key: wk_abc123
host: https://events.example.com
consent_mode: opt_in
forwarder_destinations:
  - google_ads
event_conversion_map:
  signup: CompleteRegistration
`)

	c := newProvisionClient(ts.URL, "key", false)
	if err := provisionTracker(c, 12, siteDir); err != nil {
		t.Fatalf("provisionTracker: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("got %d call(s), want 1: %+v", len(calls), calls)
	}
	call := calls[0]
	if call.Method != http.MethodPut || call.Path != "/api/sites/12/settings/tracker" {
		t.Errorf("got %s %s", call.Method, call.Path)
	}
	if call.Body["write_key"] != "wk_abc123" {
		t.Errorf("write_key = %v", call.Body["write_key"])
	}
	if call.Body["enabled"] != true {
		t.Errorf("enabled = %v", call.Body["enabled"])
	}
	if call.Body["consent_mode"] != "opt_in" {
		t.Errorf("consent_mode = %v", call.Body["consent_mode"])
	}
	m, ok := call.Body["event_conversion_map"].(map[string]any)
	if !ok || m["signup"] != "CompleteRegistration" {
		t.Errorf("event_conversion_map = %v", call.Body["event_conversion_map"])
	}
}

func TestProvisionTrackerDryRunNoWrites(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nwrite_key: wk_abc123\n")

	c := newProvisionClient(ts.URL, "key", true) // dry-run
	if err := provisionTracker(c, 12, siteDir); err != nil {
		t.Fatalf("dry-run provisionTracker: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("dry-run issued %d call(s), want 0: %+v", len(calls), calls)
	}
}

func TestProvisionTrackerMissingFileIsNoOp(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := provisionTracker(c, 12, t.TempDir()); err != nil {
		t.Fatalf("missing tracker.yaml should be a no-op, got %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("missing tracker.yaml issued %d call(s)", len(calls))
	}
}

// An enabled tracker with no write key would deploy cleanly and then silently
// drop every event, so it must fail at provision time instead.
func TestProvisionTrackerRejectsEnabledWithoutWriteKey(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nhost: https://events.example.com\n")

	c := newProvisionClient(ts.URL, "key", false)
	if err := provisionTracker(c, 12, siteDir); err == nil {
		t.Fatal("expected an error for enabled tracker without write_key")
	}
	if len(calls) != 0 {
		t.Errorf("invalid config still issued %d call(s)", len(calls))
	}
}
