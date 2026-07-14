package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dashboardsTestServer serves one dashboard definition in the wire shapes the
// real API uses: a trimmed list record, and a detail record whose JSON fields
// are JSON-encoded strings rather than raw objects.
//
// Every non-GET request is recorded so a test can assert that a run issued zero
// writes.
type dashboardsTestServer struct {
	mu     sync.Mutex
	writes []string
	detail map[string]any
	list   []map[string]any
}

func (s *dashboardsTestServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			s.mu.Lock()
			s.writes = append(s.writes, r.Method+" "+r.URL.Path)
			s.mu.Unlock()
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/admin/dashboard-definitions":
			_ = json.NewEncoder(w).Encode(map[string]any{"definitions": s.list})
		case strings.HasPrefix(r.URL.Path, "/api/admin/dashboard-definitions/"):
			_ = json.NewEncoder(w).Encode(s.detail)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *dashboardsTestServer) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// remoteDashboardDetail is the detail payload the server returns. Note that
// columns/filters/default_sort/layout/client_group_filter come back as
// JSON-encoded strings, and static filters come back under "static_filters".
func remoteDashboardDetail() map[string]any {
	return map[string]any{
		"id":                   7,
		"slug":                 "orders-daily",
		"name":                 "Orders Daily",
		"description":          "Daily order volume",
		"source_view":          "analytics.mart_orders_daily",
		"columns":              `[{"field":"day","label":"Day"},{"field":"orders","label":"Orders"}]`,
		"filters":              `[{"field":"day","type":"date_range"}]`,
		"default_chart":        "table",
		"default_sort":         `{"field":"day","dir":"desc"}`,
		"layout":               `{"rows":2}`,
		"max_rows":             5000,
		"position":             3,
		"hidden_from_overview": false,
		"export_enabled":       true,
		"static_filters":       `{"region":"eu"}`,
		"client_group_filter":  `{"column":"client_name"}`,
		"breadcrumb":           "Analytics / Orders",
	}
}

// TestDashboardsPullApplyRoundTrip is the load-bearing test for the whole diff
// story: pull a dashboard from the server, then apply the file that pull wrote,
// and assert the apply is a NOOP that issues zero writes. If apply ever stops
// diffing and starts blindly PUTting, this fails.
func TestDashboardsPullApplyRoundTrip(t *testing.T) {
	srv := &dashboardsTestServer{
		detail: remoteDashboardDetail(),
		list:   []map[string]any{{"id": 7, "slug": "orders-daily"}},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	dir := t.TempDir()
	dashDir := filepath.Join(dir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pull only refreshes slugs that are already tracked locally, so seed a stub.
	stub := filepath.Join(dashDir, "orders-daily.json")
	if err := os.WriteFile(stub, []byte(`{"slug":"orders-daily"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newProvisionClient(ts.URL, "key", false)
	if err := pullProvisionDashboards(c, 1, dashDir, false); err != nil {
		t.Fatalf("pull: %v", err)
	}

	pulled, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	// The pulled file must be canonical JSON, not the string-encoded wire form.
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(pulled, &onDisk); err != nil {
		t.Fatalf("pulled file is not valid JSON: %v", err)
	}
	if got := string(onDisk["columns"]); !strings.HasPrefix(got, "[") {
		t.Errorf("columns should be decoded to a raw array on disk, got %s", got)
	}
	if _, ok := onDisk["source_filter"]; !ok {
		t.Error("pulled file dropped static filters (expected them under source_filter)")
	}
	if _, ok := onDisk["breadcrumb"]; !ok {
		t.Error("pulled file dropped breadcrumb")
	}

	// Now apply the file we just pulled: nothing changed, so nothing may be written.
	drift, err := applyDashboards(c, dir, 1, 99, false, "")
	if err != nil {
		t.Fatalf("apply after pull: %v", err)
	}
	if drift != 0 {
		t.Errorf("round-trip apply reported drift=%d, want 0 (pull → apply must be a NOOP)", drift)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("round-trip apply issued %d write(s), want 0", n)
	}
}

// TestApplyDashboardsUpdatesOnRealChange is the counterpart: when a field really
// differs, apply must issue exactly one PUT. Without it, the NOOP test above
// would pass even if apply never wrote anything at all.
func TestApplyDashboardsUpdatesOnRealChange(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/dashboard-definitions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"definitions": []map[string]any{{"id": 7, "slug": "orders-daily"}},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(remoteDashboardDetail())
		case r.Method == http.MethodPut:
			mu.Lock()
			puts = append(puts, r.URL.Path)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	dir := t.TempDir()
	dashDir := filepath.Join(dir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same as remote except the name.
	spec := remoteDashboardFileSpec()
	spec["name"] = "Orders Daily (renamed)"
	writeJSONFile(t, filepath.Join(dashDir, "orders-daily.json"), spec)

	c := newProvisionClient(ts.URL, "key", false)
	drift, err := applyDashboards(c, dir, 1, 99, false, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if drift != 1 {
		t.Errorf("drift=%d, want 1", drift)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(puts) != 1 {
		t.Fatalf("got %d PUT(s), want 1: %v", len(puts), puts)
	}
	if puts[0] != "/api/admin/dashboard-definitions/7" {
		t.Errorf("PUT path = %s", puts[0])
	}
}

// TestApplyDashboardsDryRunNoWrites: --dry-run must never touch the server, even
// when the spec genuinely differs.
func TestApplyDashboardsDryRunNoWrites(t *testing.T) {
	srv := &dashboardsTestServer{
		detail: remoteDashboardDetail(),
		list:   []map[string]any{{"id": 7, "slug": "orders-daily"}},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	dir := t.TempDir()
	dashDir := filepath.Join(dir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := remoteDashboardFileSpec()
	spec["name"] = "Changed"
	spec["position"] = 9
	writeJSONFile(t, filepath.Join(dashDir, "orders-daily.json"), spec)
	// Also a brand new dashboard, which would otherwise POST.
	writeJSONFile(t, filepath.Join(dashDir, "new-one.json"), map[string]any{
		"slug": "new-one", "name": "New", "source_view": "v", "columns": []any{},
	})

	c := newProvisionClient(ts.URL, "key", true) // dry-run
	drift, err := applyDashboards(c, dir, 1, 99, false, "")
	if err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if drift != 1 {
		t.Errorf("dry-run drift=%d, want 1 (the changed dashboard)", drift)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("dry-run issued %d write(s), want 0", n)
	}
}

func TestDiffFieldsCoercesAPIDefaults(t *testing.T) {
	// A spec that omits default_chart and max_rows must not read as drift
	// against a server that filled in its defaults.
	local := provisionDashboardDef{Slug: "s", Name: "n", SourceView: "v"}
	remote := provisionDashboardDef{Slug: "s", Name: "n", SourceView: "v", DefaultChart: "table", MaxRows: 5000}
	if diffs := diffFields(local, remote); len(diffs) != 0 {
		t.Errorf("omitted-defaults spec reported drift: %v", diffs)
	}

	remote.DefaultChart = "bar"
	if diffs := diffFields(local, remote); len(diffs) != 1 || diffs[0] != "default_chart" {
		t.Errorf("want [default_chart], got %v", diffs)
	}
}

func TestDiffFieldsStaticFilterFormsAreEquivalent(t *testing.T) {
	local := provisionDashboardDef{StaticFilterFile: json.RawMessage(`{"region":"eu"}`)}
	remote := provisionDashboardDef{StaticFilterRemote: `{"region": "eu"}`}
	if diffs := diffFields(local, remote); len(diffs) != 0 {
		t.Errorf("file-form and wire-form static filters should compare equal, got %v", diffs)
	}

	// Empty object on the wire equals absent on disk.
	if diffs := diffFields(provisionDashboardDef{}, provisionDashboardDef{StaticFilterRemote: "{}"}); len(diffs) != 0 {
		t.Errorf(`"{}" should be treated as absent, got %v`, diffs)
	}
}

func TestDecodeRemoteStringLeavesPlainStringsAlone(t *testing.T) {
	// A field whose content is not itself JSON must survive unchanged rather
	// than becoming an invalid RawMessage.
	got := decodeRemoteString(json.RawMessage(`"hello"`))
	if string(got) != `"hello"` {
		t.Errorf("got %s, want \"hello\"", got)
	}
	if got := decodeRemoteString(json.RawMessage(`"[1,2]"`)); string(got) != "[1,2]" {
		t.Errorf("got %s, want [1,2]", got)
	}
}

// remoteDashboardFileSpec is the on-disk equivalent of remoteDashboardDetail:
// same values, canonical (non-string-encoded) JSON, static filters under
// source_filter.
func remoteDashboardFileSpec() map[string]any {
	return map[string]any{
		"slug":                 "orders-daily",
		"name":                 "Orders Daily",
		"description":          "Daily order volume",
		"source_view":          "analytics.mart_orders_daily",
		"columns":              json.RawMessage(`[{"field":"day","label":"Day"},{"field":"orders","label":"Orders"}]`),
		"filters":              json.RawMessage(`[{"field":"day","type":"date_range"}]`),
		"default_chart":        "table",
		"default_sort":         json.RawMessage(`{"field":"day","dir":"desc"}`),
		"layout":               json.RawMessage(`{"rows":2}`),
		"max_rows":             5000,
		"position":             3,
		"hidden_from_overview": false,
		"export_enabled":       true,
		"source_filter":        json.RawMessage(`{"region":"eu"}`),
		"client_group_filter":  json.RawMessage(`{"column":"client_name"}`),
		"breadcrumb":           "Analytics / Orders",
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(fmt.Errorf("write %s: %w", path, err))
	}
}
