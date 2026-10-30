package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/httpclient"
)

// rotatingServer mimics Studio's refresh endpoint strictly: every refresh
// rotates the token, and presenting an already-rotated token is treated as
// theft: every session is revoked and the call gets a 401. (The real server
// tolerates a re-sent token for 60s; this fake has no grace, so any second
// use of a token fails the test.)
type rotatingServer struct {
	mu       sync.Mutex
	live     map[string]bool
	seen     map[string]bool
	n        int
	calls    atomic.Int32
	revoked  atomic.Bool
	beforeOK func() // runs before a successful rotation responds
}

func newRotatingServer(initial string) *rotatingServer {
	return &rotatingServer{live: map[string]bool{initial: true}, seen: map[string]bool{}}
}

func (s *rotatingServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/cli/token/refresh" {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.calls.Add(1)
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	if !s.live[req.RefreshToken] {
		if s.seen[req.RefreshToken] {
			s.revoked.Store(true) // reuse detection: kill the whole family
			s.live = map[string]bool{}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	delete(s.live, req.RefreshToken)
	s.seen[req.RefreshToken] = true
	s.n++
	next := fmt.Sprintf("r%d", s.n)
	s.live[next] = true
	s.mu.Unlock()
	if s.beforeOK != nil {
		s.beforeOK()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "a-" + next,
		"refresh_token": next,
		"expires_at":    time.Now().Add(time.Hour),
	})
}

func seedNearExpiry(t *testing.T, serverURL, refresh string) {
	t.Helper()
	c := &auth.Credentials{
		AccessToken:          "stale",
		RefreshToken:         refresh,
		AccessTokenExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:            time.Now().Add(time.Minute),
	}
	c.BindTo(serverURL)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

// Many taufinity processes (MCP bridges, `auth token` from a scheduler) wake
// up near expiry at the same moment. Exactly one may refresh; the rest must
// pick up its result instead of re-sending the old token.
func TestConcurrentRenewalRefreshesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rs := newRotatingServer("r0")
	server := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer server.Close()
	seedNearExpiry(t, server.URL, "r0")

	const n = 12
	var wg sync.WaitGroup
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = New(server.URL).Token(context.Background()) // a client per "process"
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if toks[i] != "a-r1" {
			t.Fatalf("caller %d got %q, want the one renewed token a-r1", i, toks[i])
		}
	}
	if c := rs.calls.Load(); c != 1 {
		t.Fatalf("refresh calls = %d, want 1", c)
	}
	if rs.revoked.Load() {
		t.Fatal("server saw a rotated token re-sent and revoked every session")
	}
	if _, err := auth.LoadCredentialsFor(server.URL); err != nil {
		t.Fatalf("credentials gone: %v", err)
	}
}

// Another process finished the refresh while we waited for the lock: use its
// pair, send nothing.
func TestRenewalUsesRefreshAnotherProcessCompleted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rs := newRotatingServer("r0")
	server := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer server.Close()
	seedNearExpiry(t, server.URL, "r0")

	path := auth.CredentialsPathFor(server.URL)
	unlock, err := auth.LockRefresh(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		tok string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		tok, err := New(server.URL).Token(context.Background())
		ch <- result{tok, err}
	}()
	time.Sleep(150 * time.Millisecond) // the client is now waiting on the lock

	other := &auth.Credentials{AccessToken: "a-other", RefreshToken: "r-other",
		AccessTokenExpiresAt: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
	other.BindTo(server.URL)
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	unlock()

	res := <-ch
	if res.err != nil || res.tok != "a-other" {
		t.Fatalf("got (%q, %v), want the other process's token", res.tok, res.err)
	}
	if c := rs.calls.Load(); c != 0 {
		t.Fatalf("refresh calls = %d; the stale token must not be sent after another process renewed", c)
	}
}

// An older taufinity without the lock rotated the token between our read and
// our request, so ours gets a 401. Its fresh pair on disk must survive.
func TestRejectedRefreshKeepsPairRotatedByAnotherProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fresh := &auth.Credentials{AccessToken: "a-old-binary", RefreshToken: "r-old-binary",
			AccessTokenExpiresAt: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
		fresh.BindTo(serverURL)
		_ = fresh.Save()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	serverURL = server.URL
	seedNearExpiry(t, server.URL, "r0")

	tok, err := New(server.URL).Token(context.Background())
	if err != nil || tok != "a-old-binary" {
		t.Fatalf("got (%q, %v), want the pair the other process saved", tok, err)
	}
	if _, err := auth.LoadCredentialsFor(server.URL); err != nil {
		t.Fatalf("the other process's credentials were deleted: %v", err)
	}
}

// A staging session being rejected must leave production alone. Before
// credentials were per host, the staging MCP bridge renewed production's
// refresh token against staging, got a 401 and deleted production's file.
func TestStagingRejectionLeavesProductionLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	staging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer staging.Close()

	prod := &auth.Credentials{AccessToken: "prod-a", RefreshToken: "prod-r",
		AccessTokenExpiresAt: time.Now().Add(time.Minute), ExpiresAt: time.Now().Add(time.Minute)}
	prod.BindTo("https://studio.taufinity.io")
	if err := prod.Save(); err != nil {
		t.Fatal(err)
	}

	// No staging login at all: not logged in, production untouched.
	if _, err := New(staging.URL).Token(context.Background()); err == nil {
		t.Fatal("staging with no staging login should not be logged in")
	}
	// A staging login that the server rejects: staging file goes, prod stays.
	seedNearExpiry(t, staging.URL, "stg-r")
	if _, err := New(staging.URL).Token(context.Background()); err == nil {
		t.Fatal("expected the staging rejection")
	}
	if _, err := auth.LoadCredentialsFor(staging.URL); err == nil {
		t.Fatal("the rejected staging session should be removed")
	}
	got, err := auth.LoadCredentialsFor("https://studio.taufinity.io")
	if err != nil || got.RefreshToken != "prod-r" {
		t.Fatalf("production session lost: %v", err)
	}
}

// Every refresh attempt, retry included, must land inside the server's 60s
// rotation grace: a retry after a lost response re-sends the old token, and
// past the grace window the server revokes every session.
func TestRefreshRetriesStayInsideRotationGrace(t *testing.T) {
	cfg := refreshHTTPConfig(httpclient.DefaultConfig())
	worst := time.Duration(cfg.MaxRetries+1)*cfg.Timeout + time.Duration(cfg.MaxRetries)*cfg.MaxBackoff
	if worst >= 60*time.Second {
		t.Fatalf("worst-case refresh %s reaches the 60s grace window", worst)
	}
	if worst >= auth.RefreshLockMaxWait {
		t.Fatalf("worst-case refresh %s exceeds the lock wait %s; waiters would give up", worst, auth.RefreshLockMaxWait)
	}
}
