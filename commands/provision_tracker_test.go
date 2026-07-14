package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// writeWorkspaceConfig writes a workspace-config fixture. It is an
// infrastructure template, not plain JSON — note the ${...} placeholder, which
// is exactly why the write keys are matched with a regex rather than parsed.
func writeWorkspaceConfig(t *testing.T, keys ...string) string {
	t.Helper()
	var sources string
	for i, k := range keys {
		if i > 0 {
			sources += ",\n"
		}
		sources += `    {"id": "src-` + k + `", "writeKey": "` + k + `", "enabled": true}`
	}
	body := `{
  "workspaceId": "${workspace_id}",
  "sources": [
` + sources + `
  ]
}`
	path := filepath.Join(t.TempDir(), "workspaceConfig.json.tftpl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A key that IS a declared source passes and is pushed.
func TestProvisionTrackerAcceptsKnownWriteKey(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nwrite_key: wk_abc123\n")

	c := newProvisionClient(ts.URL, "key", false)
	c.workspaceConfigPath = writeWorkspaceConfig(t, "wk_other", "wk_abc123")

	if err := provisionTracker(c, 12, siteDir); err != nil {
		t.Fatalf("known write_key should pass validation, got: %v", err)
	}
	if c.WarningCount() != 0 {
		t.Errorf("a validated key should not warn, got: %v", c.warnings)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("got %d call(s), want 1", len(calls))
	}
}

// A key that is NOT a declared source means the collector 401s every event. The
// tracker would deploy clean and drop all data, so this is a hard failure, and
// nothing may be written.
func TestProvisionTrackerRejectsUnknownWriteKey(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nwrite_key: wk_typo\n")

	c := newProvisionClient(ts.URL, "key", false)
	c.workspaceConfigPath = writeWorkspaceConfig(t, "wk_abc123")

	err := provisionTracker(c, 12, siteDir)
	if err == nil {
		t.Fatal("expected an unknown write_key to be a hard failure")
	}
	if !strings.Contains(err.Error(), "wk_typo") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("an unknown write_key still issued %d call(s)", len(calls))
	}
}

// No workspace config supplied: provision still applies, but it must say out
// loud that the key went unchecked and what that costs. A safety check that
// skips itself in silence is how this bug class survives.
func TestProvisionTrackerWarnsWhenWriteKeyCannotBeValidated(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nwrite_key: wk_abc123\n")

	c := newProvisionClient(ts.URL, "key", false)
	c.workspaceConfigPath = "" // not supplied

	if err := provisionTracker(c, 12, siteDir); err != nil {
		t.Fatalf("missing workspace config should warn, not fail: %v", err)
	}
	if c.WarningCount() != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", c.WarningCount(), c.warnings)
	}
	warning := c.warnings[0]
	for _, want := range []string{"could not be validated", "--workspace-config", "silently drop every event"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning is missing %q:\n%s", want, warning)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Errorf("got %d call(s), want 1 — the unvalidated config should still apply", len(calls))
	}
}

// A --workspace-config path that does not exist is an operator mistake. Falling
// back to the unvalidated path would defeat the point of passing the flag.
func TestProvisionTrackerFailsOnMissingWorkspaceConfigFile(t *testing.T) {
	var mu sync.Mutex
	var calls []trackerCall
	ts := trackerServer(t, &calls, &mu)
	defer ts.Close()

	siteDir := writeTrackerYAML(t, t.TempDir(), "enabled: true\nwrite_key: wk_abc123\n")

	c := newProvisionClient(ts.URL, "key", false)
	c.workspaceConfigPath = filepath.Join(t.TempDir(), "nope.json.tftpl")

	if err := provisionTracker(c, 12, siteDir); err == nil {
		t.Fatal("expected a supplied-but-missing workspace config to fail")
	}
	if len(calls) != 0 {
		t.Errorf("still issued %d call(s)", len(calls))
	}
}

func TestWriteKeysInWorkspaceConfigParsesTemplate(t *testing.T) {
	// Templated infra file: not valid JSON, so the keys are matched literally.
	const tmpl = `{
  "workspaceId": "${workspace_id}",
  "sources": [
    {"writeKey": "wk_one"},
    {"writeKey" : "wk_two"}
  ],
  "destinations": [{"config": {"projectId": "${project}"}}]
}`
	got := writeKeysInWorkspaceConfig(tmpl)
	if len(got) != 2 || got[0] != "wk_one" || got[1] != "wk_two" {
		t.Errorf("got %v, want [wk_one wk_two]", got)
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
