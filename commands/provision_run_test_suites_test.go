package commands

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// A completed run is only a pass when every case passed. A case the server
// could not execute counts in error_count, not failed_count; before this the
// gate printed "PASS (5/15)" for a run where ten cases never produced a verdict.
func TestRunTestSuitesFailsOnIncompleteRun(t *testing.T) {
	for name, run := range map[string]map[string]any{
		"errored cases":   {"uuid": "u1", "status": "completed", "passed_count": 5, "failed_count": 0, "error_count": 10, "total_count": 15},
		"missing verdict": {"uuid": "u1", "status": "completed", "passed_count": 5, "failed_count": 0, "total_count": 15},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &suiteRunServer{
				suites:   []map[string]any{{"id": 1, "uuid": "u1", "name": "recipes", "slug": "recipes-must"}},
				outcomes: map[string]map[string]any{"u1": run},
			}
			ts := httptest.NewServer(srv.handler(t))
			defer ts.Close()

			c := newProvisionClient(ts.URL, "key", false)
			if err := runTestSuites(c, 1, "acme", "all", 5*time.Second); err == nil {
				t.Fatal("expected an error when not every case passed")
			}
		})
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

// flakyPolls wraps a handler so the first n poll requests (GET /api/test-runs/)
// fail the way a service does while it rolls over: mode "drop" closes the
// connection without a response, mode "503" answers 503. Other routes pass through.
func flakyPolls(next http.Handler, n int, mode string, polls *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/test-runs/") {
			if int(atomic.AddInt32(polls, 1)) <= n {
				if mode == "503" {
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func fastTransportRetry(t *testing.T, budget time.Duration) {
	t.Helper()
	oi, ob, obo, om, ot := runSuitesInterval, runSuitesTransportBudget, runSuitesTransportBackoff, runSuitesTransportMaxWait, runSuitesTransportTotalBudget
	runSuitesInterval, runSuitesTransportBudget, runSuitesTransportTotalBudget = time.Millisecond, budget, budget
	runSuitesTransportBackoff, runSuitesTransportMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		runSuitesInterval, runSuitesTransportBudget, runSuitesTransportBackoff, runSuitesTransportMaxWait, runSuitesTransportTotalBudget = oi, ob, obo, om, ot
	})
}

// A connection reset while polling (the revision rollover) must be retried and
// the passed run reported as passed.
func TestRunTestSuitesRetriesTransportFailures(t *testing.T) {
	for _, mode := range []string{"drop", "503"} {
		t.Run(mode, func(t *testing.T) {
			fastTransportRetry(t, 5*time.Second)
			srv := &suiteRunServer{
				suites:   []map[string]any{{"id": 1, "uuid": "u1", "name": "must", "slug": "must__a"}},
				outcomes: map[string]map[string]any{"u1": {"uuid": "u1", "status": "completed", "passed_count": 11, "total_count": 11}},
			}
			var polls int32
			ts := httptest.NewServer(flakyPolls(srv.handler(t), 3, mode, &polls))
			defer ts.Close()

			c := newProvisionClient(ts.URL, "key", false)
			if err := runTestSuites(c, 1, "acme", "all", 5*time.Second); err != nil {
				t.Fatalf("transport failures must be retried, got: %v", err)
			}
			if got := atomic.LoadInt32(&polls); got != 4 {
				t.Errorf("polls = %d, want 4 (3 dropped + 1 answered)", got)
			}
		})
	}
}

// A genuine failing suite behind a flaky transport still fails, and says it
// failed as a suite, not as a transport problem.
func TestRunTestSuitesRealFailureStillFailsAfterRetries(t *testing.T) {
	fastTransportRetry(t, 5*time.Second)
	srv := &suiteRunServer{
		suites:   []map[string]any{{"id": 1, "uuid": "u1", "name": "must", "slug": "must__a"}},
		outcomes: map[string]map[string]any{"u1": {"uuid": "u1", "status": "completed", "passed_count": 10, "failed_count": 1, "total_count": 11}},
	}
	var polls int32
	ts := httptest.NewServer(flakyPolls(srv.handler(t), 2, "drop", &polls))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	err := runTestSuites(c, 1, "acme", "all", 5*time.Second)
	if err == nil {
		t.Fatal("a failing suite must still fail the gate")
	}
	var run testSuiteRun
	perr := pollUntilDone(c, 1, "u1", 5*time.Second, &run)
	if perr != nil || run.allPassed() {
		t.Fatalf("poll should deliver the real result (err=%v allPassed=%v)", perr, run.allPassed())
	}
}

// A poll that never recovers fails within the budget, as a transport failure.
func TestPollUntilDoneGivesUpAsTransportFailure(t *testing.T) {
	fastTransportRetry(t, 50*time.Millisecond)
	srv := &suiteRunServer{outcomes: map[string]map[string]any{"u1": {"uuid": "u1", "status": "completed", "passed_count": 1, "total_count": 1}}}
	var polls int32
	ts := httptest.NewServer(flakyPolls(srv.handler(t), 1<<30, "drop", &polls))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	var run testSuiteRun
	start := time.Now()
	err := pollUntilDone(c, 1, "u1", 5*time.Second, &run)
	var terr *transportError
	if !errors.As(err, &terr) {
		t.Fatalf("want *transportError, got %v", err)
	}
	if !strings.Contains(err.Error(), "TRANSPORT FAILURE, not a suite result") {
		t.Errorf("message must say it was a transport failure: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("gave up too slowly: %s", time.Since(start))
	}
}

// Non-transient HTTP errors are not retried: a 401 is a real, immediate failure.
func TestPollUntilDoneDoesNotRetryAuthErrors(t *testing.T) {
	fastTransportRetry(t, 5*time.Second)
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	var run testSuiteRun
	err := pollUntilDone(c, 1, "u1", 5*time.Second, &run)
	var terr *transportError
	if err == nil || errors.As(err, &terr) || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("401 must fail once, not as transport failure: err=%v calls=%d", err, calls)
	}
}

// The outage wait must not count against the suite timeout: an outage longer
// than --timeout, followed by a run that is still going and then completes, is
// a pass. Removing the deadline extension turns this red.
func TestPollUntilDoneOutageDoesNotConsumeSuiteTimeout(t *testing.T) {
	fastTransportRetry(t, 5*time.Second)
	runSuitesTransportBackoff, runSuitesTransportMaxWait = 20*time.Millisecond, 20*time.Millisecond
	var polls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		if n <= 6 { // about 120ms of outage, longer than the 60ms suite timeout
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		status := "running"
		if n >= 9 {
			status = "completed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"uuid": "u1", "status": status, "passed_count": 1, "total_count": 1}})
	}))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	var run testSuiteRun
	if err := pollUntilDone(c, 1, "u1", 60*time.Millisecond, &run); err != nil {
		t.Fatalf("an outage must not consume the suite timeout: %v", err)
	}
	if !run.allPassed() {
		t.Errorf("run = %+v, want passed", run)
	}
}

// A flapping API (one bad poll, one good poll, repeat) must hit the overall cap
// rather than extend the wait forever, and fail as a transport failure.
func TestPollUntilDoneFlappingAPIHitsTotalCap(t *testing.T) {
	fastTransportRetry(t, 5*time.Second)
	runSuitesTransportTotalBudget = 100 * time.Millisecond
	runSuitesTransportBackoff, runSuitesTransportMaxWait = 20*time.Millisecond, 20*time.Millisecond
	var polls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1)%2 == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"uuid": "u1", "status": "running"}})
	}))
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	var run testSuiteRun
	start := time.Now()
	err := pollUntilDone(c, 1, "u1", 5*time.Second, &run)
	var terr *transportError
	if !errors.As(err, &terr) {
		t.Fatalf("want *transportError from the total cap, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("flapping API extended the wait: %s", time.Since(start))
	}
}
