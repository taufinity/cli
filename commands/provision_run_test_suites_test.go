package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// suiteRunServer fakes the trigger + poll cycle. Each suite reports the
// pass/fail counts configured in outcomes, keyed by suite UUID.
type suiteRunServer struct {
	mu       sync.Mutex
	suites   []map[string]any
	outcomes map[string]map[string]any // uuid → run payload
	triggers []string
}

func (s *suiteRunServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/test-suites/":
			_ = json.NewEncoder(w).Encode(s.suites)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run"):
			uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/test-suites/"), "/run")
			s.mu.Lock()
			s.triggers = append(s.triggers, uuid)
			s.mu.Unlock()
			// The trigger returns a queued run; the poll returns the outcome.
			_ = json.NewEncoder(w).Encode(map[string]any{"uuid": uuid, "status": "running"})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/test-runs/"):
			uuid := strings.TrimPrefix(r.URL.Path, "/api/test-runs/")
			_ = json.NewEncoder(w).Encode(map[string]any{"run": s.outcomes[uuid]})

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func (s *suiteRunServer) triggered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.triggers...)
}

func TestRunTestSuitesAllPass(t *testing.T) {
	srv := &suiteRunServer{
		suites: []map[string]any{
			{"id": 1, "uuid": "u1", "name": "must__formula-sanity", "slug": "must__formula-sanity"},
			{"id": 2, "uuid": "u2", "name": "coverage", "slug": "coverage-optional"},
		},
		outcomes: map[string]map[string]any{
			"u1": {"uuid": "u1", "status": "completed", "passed_count": 4, "total_count": 4},
			"u2": {"uuid": "u2", "status": "completed", "passed_count": 2, "total_count": 2},
		},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := runTestSuites(c, 1, "acme", "all", 5*time.Second); err != nil {
		t.Fatalf("runTestSuites: %v", err)
	}
	if got := srv.triggered(); len(got) != 2 {
		t.Errorf("triggered %v, want both suites", got)
	}
}

// A failing case must make the command fail, otherwise it is useless as a CI gate.
func TestRunTestSuitesFailsOnFailingCase(t *testing.T) {
	srv := &suiteRunServer{
		suites: []map[string]any{{"id": 1, "uuid": "u1", "name": "orders", "slug": "orders-must"}},
		outcomes: map[string]map[string]any{
			"u1": {"uuid": "u1", "status": "completed", "passed_count": 3, "failed_count": 1, "total_count": 4},
		},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := runTestSuites(c, 1, "acme", "all", 5*time.Second); err == nil {
		t.Fatal("expected an error when a suite has a failing case")
	}
}

// A run that ends in a non-completed terminal state is a failure too — otherwise
// an errored run would read as a pass.
func TestRunTestSuitesFailsOnErroredRun(t *testing.T) {
	srv := &suiteRunServer{
		suites: []map[string]any{{"id": 1, "uuid": "u1", "name": "orders", "slug": "orders"}},
		outcomes: map[string]map[string]any{
			"u1": {"uuid": "u1", "status": "failed", "error": "provider timeout"},
		},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := runTestSuites(c, 1, "acme", "all", 5*time.Second); err == nil {
		t.Fatal("expected an error when a run ends with status=failed")
	}
}

func TestRunTestSuitesPriorityFilter(t *testing.T) {
	srv := &suiteRunServer{
		suites: []map[string]any{
			{"id": 1, "uuid": "u1", "name": "critical", "slug": "must__critical"},
			{"id": 2, "uuid": "u2", "name": "nice", "slug": "nice-optional"},
			{"id": 3, "uuid": "u3", "name": "plain", "slug": "plain"},
		},
		outcomes: map[string]map[string]any{
			"u1": {"uuid": "u1", "status": "completed", "passed_count": 1, "total_count": 1},
		},
	}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := runTestSuites(c, 1, "acme", "must", 5*time.Second); err != nil {
		t.Fatalf("runTestSuites: %v", err)
	}
	got := srv.triggered()
	if len(got) != 1 || got[0] != "u1" {
		t.Errorf("priority=must triggered %v, want only [u1]", got)
	}
}

func TestRunTestSuitesNoSuitesIsNotAnError(t *testing.T) {
	srv := &suiteRunServer{suites: []map[string]any{}}
	ts := httptest.NewServer(srv.handler(t))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := runTestSuites(c, 1, "acme", "all", time.Second); err != nil {
		t.Fatalf("an org with no suites should not be an error, got %v", err)
	}
}

func TestSlugPriorityOf(t *testing.T) {
	cases := map[string]string{
		"must__formula-sanity":       "must",
		"formula-sanity-must":        "must",
		"formula-sanity-must-a1b2c3": "must",
		"coverage-optional":          "optional",
		"recommend__nice-to-have":    "recommend",
		"plain-slug":                 "recommend", // no priority encoded → default
	}
	for slug, want := range cases {
		if got := slugPriorityOf(slug); got != want {
			t.Errorf("slugPriorityOf(%q) = %q, want %q", slug, got, want)
		}
	}
}
