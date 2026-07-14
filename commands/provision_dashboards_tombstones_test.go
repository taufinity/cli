package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tombstoneServer serves a dashboard list containing the two slugs below and
// records every request, so a test can assert on the exact DELETEs issued.
type tombstoneServer struct {
	mu       sync.Mutex
	requests []string
	list     []map[string]any
}

func (s *tombstoneServer) record(method, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, method+" "+path)
}

func (s *tombstoneServer) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}
	return out
}

func (s *tombstoneServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/admin/dashboard-definitions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"definitions": s.list})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeTombstones drops a _tombstones.json into a fresh <dir>/dashboards and
// returns the parent dir (what applyDashboards takes) and the dashboards dir.
func writeTombstones(t *testing.T, entries []map[string]string) (dir, dashDir string) {
	t.Helper()
	dir = t.TempDir()
	dashDir = filepath.Join(dir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dashDir, tombstonesFileName), map[string]any{"slugs": entries})
	return dir, dashDir
}

// A tombstoned slug that exists on the server must be DELETEd — provision had
// no way to remove a dashboard at all before this.
func TestTombstoneDeletesSlugPresentOnServer(t *testing.T) {
	srv := &tombstoneServer{list: []map[string]any{
		{"id": 7, "slug": "orders-daily"},
		{"id": 9, "slug": "orders-weekly"},
	}}
	ts := srv.start(t)

	dir, _ := writeTombstones(t, []map[string]string{
		{"slug": "orders-weekly", "reason": "replaced by orders-daily"},
	})

	c := newProvisionClient(ts.URL, "key", false)
	if _, err := applyDashboards(c, dir, 1, 99, false, ""); err != nil {
		t.Fatalf("apply: %v", err)
	}

	writes := srv.writes()
	if len(writes) != 1 {
		t.Fatalf("got %d write(s), want exactly 1 DELETE: %v", len(writes), writes)
	}
	if writes[0] != "DELETE /api/admin/dashboard-definitions/9" {
		t.Errorf("wrong write issued: %s", writes[0])
	}
}

// The tombstone stays in the spec after it has been applied — it is the record
// of the deletion. So a second run, where the slug is already gone, must be a
// clean no-op and not an error, or every subsequent provision would fail.
func TestTombstoneForAbsentSlugIsNoOpNotError(t *testing.T) {
	srv := &tombstoneServer{list: []map[string]any{
		{"id": 7, "slug": "orders-daily"},
	}}
	ts := srv.start(t)

	dir, _ := writeTombstones(t, []map[string]string{
		{"slug": "orders-weekly", "reason": "replaced by orders-daily"},
	})

	c := newProvisionClient(ts.URL, "key", false)
	if _, err := applyDashboards(c, dir, 1, 99, false, ""); err != nil {
		t.Fatalf("tombstone for an already-deleted slug should be a no-op, got: %v", err)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Errorf("already-gone slug still issued %d write(s): %v", len(writes), writes)
	}
}

// A destructive action must be announced, with its reason, or nobody reviews it.
func TestTombstonePrintsSlugAndReason(t *testing.T) {
	srv := &tombstoneServer{list: []map[string]any{
		{"id": 9, "slug": "orders-weekly"},
	}}
	ts := srv.start(t)

	dir, _ := writeTombstones(t, []map[string]string{
		{"slug": "orders-weekly", "reason": "replaced by orders-daily"},
	})

	c := newProvisionClient(ts.URL, "key", false)
	out := captureStdout(t, func() {
		if _, err := applyDashboards(c, dir, 1, 99, false, ""); err != nil {
			t.Fatalf("apply: %v", err)
		}
	})

	if !strings.Contains(out, "DELETE orders-weekly") {
		t.Errorf("deletion was not announced:\n%s", out)
	}
	if !strings.Contains(out, "replaced by orders-daily") {
		t.Errorf("deletion did not print its reason:\n%s", out)
	}
}

// --dry-run must preview the delete and issue zero writes. The server fails the
// test on any non-GET request.
func TestTombstoneDryRunIssuesNoWrites(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("dry-run issued a write: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/admin/dashboard-definitions" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"definitions": []map[string]any{{"id": 9, "slug": "orders-weekly"}},
			})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	dir, _ := writeTombstones(t, []map[string]string{
		{"slug": "orders-weekly", "reason": "replaced by orders-daily"},
	})

	c := newProvisionClient(ts.URL, "key", true) // dry-run
	out := captureStdout(t, func() {
		if _, err := applyDashboards(c, dir, 1, 99, false, ""); err != nil {
			t.Fatalf("dry-run apply: %v", err)
		}
	})
	if !strings.Contains(out, "DELETE orders-weekly") {
		t.Errorf("dry-run did not preview the delete:\n%s", out)
	}
}

// A missing _tombstones.json is the normal case (most customers never delete a
// dashboard), not a missing-config error.
func TestNoTombstonesFileIsNotAnError(t *testing.T) {
	got, err := readDashboardTombstones(t.TempDir())
	if err != nil {
		t.Fatalf("absent tombstones file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d tombstone(s) from an empty dir", len(got))
	}
}

// A delete with no reason is not reviewable, so it is rejected at parse time.
func TestTombstoneWithoutReasonIsRejected(t *testing.T) {
	_, dashDir := writeTombstones(t, []map[string]string{{"slug": "orders-weekly"}})
	if _, err := readDashboardTombstones(dashDir); err == nil {
		t.Fatal("expected a tombstone with no reason to be rejected")
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// printed. applyDashboards reports to stdout via fmt.Printf.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = orig
	return <-done
}
