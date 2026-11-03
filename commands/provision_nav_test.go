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

// navTestServer captures every POST /api/admin/organizations/{id}/settings
// call so a test can assert exactly which nav keys reached Studio.
type navTestServer struct {
	mu    sync.Mutex
	calls []navKV
}

func (s *navTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var kv struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(body, &kv); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.calls = append(s.calls, navKV{kv.Key, kv.Value})
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

// value returns the value written for key, and whether it was written at all.
func (s *navTestServer) value(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.Key == key {
			return c.Value, true
		}
	}
	return "", false
}

// applyNavFixture writes nav.yaml into a temp dir and provisions it.
func applyNavFixture(t *testing.T, yaml string, dryRun bool) *navTestServer {
	t.Helper()
	srv := &navTestServer{}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nav.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write nav.yaml: %v", err)
	}
	c := newProvisionClient(ts.URL, "test-key", dryRun)
	if err := applyNav(c, dir, 18); err != nil {
		t.Fatalf("applyNav: %v", err)
	}
	return srv
}

// TestProvisionNav_DefaultRouteIsWritten is the regression this file exists
// for. navBlock had no DefaultRoute field, so a customer nav.yaml declaring
// `default_route: /playbooks` parsed cleanly, provisioned "successfully", and
// never reached Studio — users kept landing on /reports with no error anywhere
// to explain why. Against the old code this test fails on the missing key.
func TestProvisionNav_DefaultRouteIsWritten(t *testing.T) {
	srv := applyNavFixture(t, "nav:\n  default_route: /playbooks\n", false)

	got, ok := srv.value("nav.default_route")
	if !ok {
		t.Fatalf("nav.default_route was never written; keys sent: %+v", srv.calls)
	}
	if got != "/playbooks" {
		t.Errorf("nav.default_route = %q, want %q", got, "/playbooks")
	}
}

// TestProvisionNav_LabelsBecomePerSlugKeys covers the other half of the gap:
// GetMyNav builds label_overrides from nav.label.<slug> keys and the sidebar
// renders them, but nothing could write those keys.
func TestProvisionNav_LabelsBecomePerSlugKeys(t *testing.T) {
	srv := applyNavFixture(t, `nav:
  labels:
    playbooks: "Offerte maken"
    exports: "Gemaakte offertes"
`, false)

	for slug, want := range map[string]string{
		"nav.label.playbooks": "Offerte maken",
		"nav.label.exports":   "Gemaakte offertes",
	} {
		got, ok := srv.value(slug)
		if !ok {
			t.Errorf("%s was never written; keys sent: %+v", slug, srv.calls)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", slug, got, want)
		}
	}
}

// TestProvisionNav_HideAndPinAreJSONArrays pins the encoding: GetMyNav
// json.Unmarshals these into []string, so a comma-joined string would parse
// as nothing and silently hide nothing.
func TestProvisionNav_HideAndPinAreJSONArrays(t *testing.T) {
	srv := applyNavFixture(t, `nav:
  hide: [test-suites, jobs]
  pin: [playbooks]
`, false)

	hide, ok := srv.value("nav.hide")
	if !ok {
		t.Fatalf("nav.hide was never written; keys sent: %+v", srv.calls)
	}
	var hideSlugs []string
	if err := json.Unmarshal([]byte(hide), &hideSlugs); err != nil {
		t.Fatalf("nav.hide %q is not a JSON array: %v", hide, err)
	}
	if len(hideSlugs) != 2 || hideSlugs[0] != "test-suites" || hideSlugs[1] != "jobs" {
		t.Errorf("nav.hide = %v, want [test-suites jobs]", hideSlugs)
	}

	pin, ok := srv.value("nav.pin")
	if !ok {
		t.Fatalf("nav.pin was never written; keys sent: %+v", srv.calls)
	}
	var pinSlugs []string
	if err := json.Unmarshal([]byte(pin), &pinSlugs); err != nil {
		t.Fatalf("nav.pin %q is not a JSON array: %v", pin, err)
	}
	if len(pinSlugs) != 1 || pinSlugs[0] != "playbooks" {
		t.Errorf("nav.pin = %v, want [playbooks]", pinSlugs)
	}
}

// TestProvisionNav_RoleDefaultsAndProfilesStillWork guards the behaviour that
// already existed, so the new flattening cannot quietly drop it.
func TestProvisionNav_RoleDefaultsAndProfilesStillWork(t *testing.T) {
	srv := applyNavFixture(t, `nav:
  role_defaults:
    owner: wvs_operator
    member: wvs_operator
  custom_profiles:
    wvs_operator:
      items: [playbooks, exports, knowledge]
`, false)

	if got, ok := srv.value("nav.role.owner"); !ok || got != "wvs_operator" {
		t.Errorf("nav.role.owner = %q (written=%v), want wvs_operator", got, ok)
	}
	if got, ok := srv.value("nav.role.member"); !ok || got != "wvs_operator" {
		t.Errorf("nav.role.member = %q (written=%v), want wvs_operator", got, ok)
	}

	raw, ok := srv.value("nav.custom_profiles")
	if !ok {
		t.Fatalf("nav.custom_profiles was never written; keys sent: %+v", srv.calls)
	}
	var profiles map[string][]string
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil {
		t.Fatalf("nav.custom_profiles is not valid JSON: %v", err)
	}
	items := profiles["wvs_operator"]
	if len(items) != 3 || items[0] != "playbooks" || items[2] != "knowledge" {
		t.Errorf("wvs_operator items = %v, want [playbooks exports knowledge]", items)
	}
}

// TestProvisionNav_OrderIsDeterministic keeps dry-run output reviewable: Go
// map iteration is randomised, so an unsorted flatten would reorder the diff
// between two runs over identical input.
func TestProvisionNav_OrderIsDeterministic(t *testing.T) {
	yaml := `nav:
  default_route: /playbooks
  role_defaults: {owner: a, admin: b, member: c, viewer: d}
  labels: {playbooks: P, exports: E, knowledge: K}
`
	var first []string
	for run := 0; run < 5; run++ {
		nav, err := parseNavYAML([]byte(yaml))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		kvs, err := navSettingKVs(nav)
		if err != nil {
			t.Fatalf("flatten: %v", err)
		}
		keys := make([]string, len(kvs))
		for i, kv := range kvs {
			keys[i] = kv.Key
		}
		if run == 0 {
			first = keys
			continue
		}
		if len(keys) != len(first) {
			t.Fatalf("run %d produced %d keys, run 0 produced %d", run, len(keys), len(first))
		}
		for i := range keys {
			if keys[i] != first[i] {
				t.Fatalf("run %d key order differs at %d: %q vs %q", run, i, keys[i], first[i])
			}
		}
	}
}

// TestProvisionNav_EmptyBlockWritesNothing: an absent key must not be written
// as an empty value, which would clear a setting the YAML never mentioned.
func TestProvisionNav_EmptyBlockWritesNothing(t *testing.T) {
	srv := applyNavFixture(t, "nav:\n  role_defaults:\n    owner: full_access\n", false)

	for _, key := range []string{"nav.default_route", "nav.hide", "nav.pin", "nav.custom_profiles"} {
		if v, ok := srv.value(key); ok {
			t.Errorf("%s should not be written when absent from YAML, got %q", key, v)
		}
	}
}

// TestProvisionNav_DryRunWritesNothing matches every other provisioner: a diff
// must not mutate. Worth asserting here because the CLI's --dry-run is already
// known to execute on some command groups.
func TestProvisionNav_DryRunWritesNothing(t *testing.T) {
	srv := applyNavFixture(t, "nav:\n  default_route: /playbooks\n  labels: {playbooks: P}\n", true)

	if len(srv.calls) != 0 {
		t.Errorf("dry-run must not write; got %d calls: %+v", len(srv.calls), srv.calls)
	}
}

// TestProvisionNav_MissingFileIsNoop keeps nav.yaml optional.
func TestProvisionNav_MissingFileIsNoop(t *testing.T) {
	srv := &navTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyNav(c, t.TempDir(), 18); err != nil {
		t.Fatalf("missing nav.yaml should be a no-op: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("missing nav.yaml should write nothing, got %+v", srv.calls)
	}
}

// TestProvisionNav_ClientGroupsIsNotSilentlyDropped: the field parses but is
// provisioned from access/client-groups/<slug>.yaml instead. It must not be
// accepted in silence — an org setting it here would otherwise believe its
// client-group nav was configured.
func TestProvisionNav_ClientGroupsWarnsAndWritesNoKeys(t *testing.T) {
	srv := applyNavFixture(t, `nav:
  default_route: /reports
  client_groups:
    acme:
      profile: client_reporting
`, false)

	for _, c := range srv.calls {
		if c.Key != "nav.default_route" {
			t.Errorf("client_groups must not produce nav keys, got %q = %q", c.Key, c.Value)
		}
	}
}
