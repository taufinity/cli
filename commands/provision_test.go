package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionClientDryRun(t *testing.T) {
	c := newProvisionClient("http://localhost:9999", "test-key", true)
	body, status, err := c.post("/test-path", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("dry-run post: %v", err)
	}
	if status != 200 {
		t.Errorf("dry-run status want 200, got %d", status)
	}
	_ = body
}

func TestResolveProvisionOrgID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/organizations" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 42, "slug": "test-org"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "test-key", false)
	id, err := resolveProvisionOrgID(c, "test-org")
	if err != nil {
		t.Fatalf("resolveProvisionOrgID: %v", err)
	}
	if id != 42 {
		t.Errorf("want id=42, got %d", id)
	}
}

func TestResolveProvisionOrgIDCaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 7, "slug": "Felix-Works"},
		})
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "key", false)
	id, err := resolveProvisionOrgID(c, "felix-works")
	if err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
	if id != 7 {
		t.Errorf("want 7, got %d", id)
	}
}

func TestApplyPortalNoFile(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	if err := applyPortal(c, dir, 1); err != nil {
		t.Fatalf("applyPortal with no file: %v", err)
	}
}

func TestApplyPortalWithFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("portal_name: Test\nportal_domain: test.example.com\nprimary_color: \"#ff0000\"\n")
	if err := os.WriteFile(filepath.Join(dir, "portal.yaml"), content, 0644); err != nil {
		t.Fatal(err)
	}
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "key", false)
	if err := applyPortal(c, dir, 42); err != nil {
		t.Fatalf("applyPortal: %v", err)
	}
	if !called {
		t.Error("expected HTTP call, got none")
	}
}

func TestApplyOrgMembersNoFile(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	if err := applyOrgMembers(c, dir, 1); err != nil {
		t.Fatalf("applyOrgMembers with no file: %v", err)
	}
}

func TestApplyKPINoFile(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	if err := applyKPI(c, dir, 1); err != nil {
		t.Fatalf("applyKPI with no file: %v", err)
	}
}

func TestApplyNavNoFile(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	if err := applyNav(c, dir, 1); err != nil {
		t.Fatalf("applyNav with no file: %v", err)
	}
}

func TestApplyProvidersNoFiles(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	id, err := applyProviders(c, dir, 1)
	if err != nil {
		t.Fatalf("applyProviders with no files: %v", err)
	}
	if id != 0 {
		t.Errorf("want providerID=0, got %d", id)
	}
}

func TestApplySitesNoDir(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	if err := applySites(c, dir, 1); err != nil {
		t.Fatalf("applySites with no sites dir: %v", err)
	}
}

func TestApplyDashboardsNoDashboardsDir(t *testing.T) {
	dir := t.TempDir()
	c := newProvisionClient("http://localhost:9999", "key", true)
	drift, err := applyDashboards(c, dir, 1, 0, false, "")
	if err != nil {
		t.Fatalf("applyDashboards with no dashboards dir: %v", err)
	}
	if drift != 0 {
		t.Errorf("want drift=0, got %d", drift)
	}
}

func TestProvisionSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"WVS Hoveniers", "wvs-hoveniers"},
		{"  leading trailing  ", "leading-trailing"},
		{"double--dash", "double-dash"},
	}
	for _, tc := range cases {
		got := provisionSlugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestYamlUnmarshalStrictRejectsUnknownKey(t *testing.T) {
	type simple struct {
		Name string `yaml:"name"`
	}
	data := []byte("name: test\nunknown_key: value\n")
	var v simple
	if err := yamlUnmarshalStrict(data, &v); err == nil {
		t.Error("expected error for unknown key, got nil")
	}
}

func TestYamlUnmarshalStrictAcceptsKnownKey(t *testing.T) {
	type simple struct {
		Name string `yaml:"name"`
	}
	data := []byte("name: test\n")
	var v simple
	if err := yamlUnmarshalStrict(data, &v); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Name != "test" {
		t.Errorf("want name=test, got %q", v.Name)
	}
}
