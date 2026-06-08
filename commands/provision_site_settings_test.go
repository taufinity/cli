package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// siteSettingsTestServer captures every PUT /api/sites/{id}/settings/{section}
// call. Lets us assert exactly which sections were pushed for which site,
// and what JSON body was sent, without standing up a real Studio.
type siteSettingsTestServer struct {
	mu    sync.Mutex
	calls []siteSettingsCall
}

type siteSettingsCall struct {
	SiteID  int
	Section string
	Body    map[string]any
}

func (s *siteSettingsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /api/sites/{id}/settings/{section}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		// expect ["api", "sites", "{id}", "settings", "{section}"]
		if len(parts) != 5 || parts[0] != "api" || parts[1] != "sites" || parts[3] != "settings" {
			http.NotFound(w, r)
			return
		}
		id, _ := strconv.Atoi(parts[2])
		section := parts[4]
		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)

		s.mu.Lock()
		s.calls = append(s.calls, siteSettingsCall{SiteID: id, Section: section, Body: body})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"Settings updated"}`))
	})
}

// TestProvisionGeneralSettings_HappyPath — a populated general-settings.yaml
// pushes a PUT with the same fields, JSON-encoded.
func TestProvisionGeneralSettings_HappyPath(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `description: Robin's personal LinkedIn channel.
enabled: true
`
	if err := os.WriteFile(filepath.Join(dir, "general-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionGeneralSettings(c, 14, dir); err != nil {
		t.Fatalf("provisionGeneralSettings: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PUT call, got %d", len(srv.calls))
	}
	got := srv.calls[0]
	if got.SiteID != 14 || got.Section != "general" {
		t.Errorf("call routing wrong: got site=%d section=%q", got.SiteID, got.Section)
	}
	if got.Body["description"] != "Robin's personal LinkedIn channel." {
		t.Errorf("description not propagated: %+v", got.Body)
	}
	if got.Body["enabled"] != true {
		t.Errorf("enabled not propagated: %+v", got.Body)
	}
}

// TestProvisionContentSettings_HappyPath — category + format + tone all push.
func TestProvisionContentSettings_HappyPath(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `category: AI engineering, fractional CTO observations
format: short-form LinkedIn post
tone: first-person hands-on engineer
`
	if err := os.WriteFile(filepath.Join(dir, "content-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionContentSettings(c, 14, dir); err != nil {
		t.Fatalf("provisionContentSettings: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(srv.calls))
	}
	got := srv.calls[0]
	if got.Section != "content" {
		t.Errorf("section: got %q want content", got.Section)
	}
	if got.Body["category"] != "AI engineering, fractional CTO observations" {
		t.Errorf("category: %+v", got.Body)
	}
	if got.Body["format"] != "short-form LinkedIn post" {
		t.Errorf("format: %+v", got.Body)
	}
}

// TestProvisionMetadataSettings_HappyPath — metadata is a free-form map, so
// arbitrary keys round-trip. target_audience is the one the topic prompt reads.
func TestProvisionMetadataSettings_HappyPath(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `target_audience: founders and CTOs evaluating AI
custom_key: forward-compat value
`
	if err := os.WriteFile(filepath.Join(dir, "metadata-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := provisionMetadataSettings(c, 14, dir); err != nil {
		t.Fatalf("provisionMetadataSettings: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(srv.calls))
	}
	got := srv.calls[0]
	if got.Section != "metadata" {
		t.Errorf("section: got %q want metadata", got.Section)
	}
	if got.Body["target_audience"] != "founders and CTOs evaluating AI" {
		t.Errorf("target_audience: %+v", got.Body)
	}
	if got.Body["custom_key"] != "forward-compat value" {
		t.Errorf("custom_key (free-form map should round-trip): %+v", got.Body)
	}
}

// TestProvisionSettings_MissingFileIsNoop — every settings file is opt-in.
// No file in the dir should be silently fine and produce zero PUT calls.
func TestProvisionSettings_MissingFileIsNoop(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir() // empty
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := provisionGeneralSettings(c, 14, dir); err != nil {
		t.Fatalf("general (missing): %v", err)
	}
	if err := provisionContentSettings(c, 14, dir); err != nil {
		t.Fatalf("content (missing): %v", err)
	}
	if err := provisionMetadataSettings(c, 14, dir); err != nil {
		t.Fatalf("metadata (missing): %v", err)
	}

	if len(srv.calls) != 0 {
		t.Errorf("missing files should produce zero PUTs, got %d: %+v", len(srv.calls), srv.calls)
	}
}

// TestProvisionGeneralSettings_DryRun — dry-run does not hit the server.
// Guards against accidentally bypassing the dry-run flag in the shared
// helper (the dry-run check lives in writeWithHeaders, not here, so this
// is mostly an integration assertion).
func TestProvisionGeneralSettings_DryRun(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "general-settings.yaml"), []byte("description: dry\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", true) // dry-run on
	if err := provisionGeneralSettings(c, 14, dir); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	if len(srv.calls) != 0 {
		t.Errorf("dry-run should not hit server, got %d calls", len(srv.calls))
	}
}

// TestProvisionGeneralSettings_MalformedYAMLErrors — a syntactically broken
// YAML file should surface as a parse error, not a silent no-op.
func TestProvisionGeneralSettings_MalformedYAMLErrors(t *testing.T) {
	srv := &siteSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	// Unclosed quote — yaml.v3 rejects this with a scanner error.
	if err := os.WriteFile(filepath.Join(dir, "general-settings.yaml"), []byte(`description: "unclosed`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := provisionGeneralSettings(c, 14, dir)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse general-settings.yaml") {
		t.Errorf("error should wrap parse step, got: %v", err)
	}
}
