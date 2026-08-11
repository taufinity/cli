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

// orgSettingsTestServer captures every PUT /api/organizations/{id}/settings
// call. Lets us assert exactly which fields were pushed, without standing
// up a real Studio.
type orgSettingsTestServer struct {
	mu    sync.Mutex
	calls []orgSettingsCall
}

type orgSettingsCall struct {
	OrgID int
	Body  map[string]any
}

func (s *orgSettingsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /api/organizations/{id}/settings
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "api" || parts[1] != "organizations" || parts[3] != "settings" {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.Atoi(parts[2])
		if err != nil {
			http.Error(w, "invalid org id", http.StatusBadRequest)
			return
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)

		s.mu.Lock()
		s.calls = append(s.calls, orgSettingsCall{OrgID: id, Body: body})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"organization_id":1}`))
	})
}

func TestProvisionOrgSettings_DefaultKnowledgeTags(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `default_knowledge_tags: [org-context]
`
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("applyOrgSettings: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PUT call, got %d", len(srv.calls))
	}
	got := srv.calls[0]
	if got.OrgID != 12 {
		t.Errorf("org id: got %d want 12", got.OrgID)
	}
	if got.Body["default_knowledge_tags"] != "org-context" {
		t.Errorf("default_knowledge_tags: got %+v, want comma-joined string \"org-context\"", got.Body)
	}
	// Fields not set in YAML must not appear in the JSON body at all —
	// the API does a partial update keyed on field presence, so sending an
	// explicit null/zero-value here would clear settings we never touched.
	if _, present := got.Body["chat_log_enabled"]; present {
		t.Errorf("chat_log_enabled should be absent (not set in YAML), got %+v", got.Body)
	}
	if _, present := got.Body["privacy_protection_enabled"]; present {
		t.Errorf("privacy_protection_enabled should be absent (not set in YAML), got %+v", got.Body)
	}
}

func TestProvisionOrgSettings_EmptyList_ClearsTag(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	// An explicit empty list is distinct from omitting the key entirely:
	// yaml.v3 unmarshals `default_knowledge_tags: []` into a non-nil empty
	// []string, while an absent key leaves the field nil. Only the former
	// must produce `"default_knowledge_tags":""` in the body (clearing the
	// server-side value) — the latter must omit the key so the server-side
	// value is left untouched. A refactor from `!= nil` to a length check
	// would silently break this distinction with nothing else failing.
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte("default_knowledge_tags: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("applyOrgSettings: %v", err)
	}
	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PUT call, got %d", len(srv.calls))
	}
	got, present := srv.calls[0].Body["default_knowledge_tags"]
	if !present {
		t.Fatal("default_knowledge_tags must be present (clearing), not omitted, for an explicit empty list")
	}
	if got != "" {
		t.Errorf("default_knowledge_tags: got %+v, want empty string (clears server-side)", got)
	}
}

func TestProvisionOrgSettings_MultipleTags_CommaJoined(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `default_knowledge_tags: [org-context, roles, skills]
`
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("applyOrgSettings: %v", err)
	}
	if srv.calls[0].Body["default_knowledge_tags"] != "org-context,roles,skills" {
		t.Errorf("default_knowledge_tags: got %+v", srv.calls[0].Body)
	}
}

func TestProvisionOrgSettings_BoolAndStringFields(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `chat_log_enabled: true
privacy_protection_enabled: false
default_homepage: "/widgets/8/chat"
allowed_email_domains: "@taufinity\\.io$"
require_portal_domain: true
`
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("applyOrgSettings: %v", err)
	}
	got := srv.calls[0].Body
	if got["chat_log_enabled"] != true {
		t.Errorf("chat_log_enabled: %+v", got)
	}
	if got["privacy_protection_enabled"] != false {
		t.Errorf("privacy_protection_enabled: %+v", got)
	}
	if got["default_homepage"] != "/widgets/8/chat" {
		t.Errorf("default_homepage: %+v", got)
	}
	if got["require_portal_domain"] != true {
		t.Errorf("require_portal_domain: %+v", got)
	}
	// default_knowledge_tags was never set in this YAML — must be absent,
	// not an empty string (empty string would clear it server-side).
	if _, present := got["default_knowledge_tags"]; present {
		t.Errorf("default_knowledge_tags should be absent, got %+v", got)
	}
}

func TestProvisionOrgSettings_MissingFileIsNoop(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir() // empty
	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("missing file should be a no-op: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("missing file should produce zero PUTs, got %d", len(srv.calls))
	}
}

func TestProvisionOrgSettings_MalformedYAMLErrors(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte(`chat_log_enabled: "unclosed`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := applyOrgSettings(c, dir, 12)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "org-settings.yaml") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestProvisionOrgSettings_EmptyYAML_NoFieldsSet_Errors(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte("# no fields set\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	err := applyOrgSettings(c, dir, 12)
	if err == nil {
		t.Fatal("expected an error for a YAML file with no fields set (matches the API's own 'no settings provided' rejection)")
	}
	if len(srv.calls) != 0 {
		t.Errorf("no fields set must not PUT at all, got %d calls", len(srv.calls))
	}
}

func TestProvisionOrgSettings_DryRun(t *testing.T) {
	srv := &orgSettingsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "org-settings.yaml"), []byte("default_knowledge_tags: [org-context]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", true) // dry-run on
	if err := applyOrgSettings(c, dir, 12); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("dry-run should not hit server, got %d calls", len(srv.calls))
	}
}
