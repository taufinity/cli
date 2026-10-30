package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

// A Studio signing-key rotation invalidates every access token at once, while
// the opaque refresh tokens stay valid. The CLI used to renew only on local
// expiry, so every user stayed logged out until a new login. These tests pin
// the fix: on a 401, renew once and retry once.

// Fake token values only.
const (
	oldAccess  = "old-access-token"
	oldRefresh = "old-refresh-token"
	newAccess  = "new-access-token"
	newRefresh = "new-refresh-token"
)

// rotatedServer accepts only newAccess on /api/*, like a server whose signing
// key changed; /api/cli/token/refresh exchanges oldRefresh for a new pair, or
// answers refreshStatus when that is set.
type rotatedServer struct {
	srv           *httptest.Server
	refreshStatus int    // 0 = succeed
	accept        string // the only bearer /api/* accepts; "" = none
	refreshes     atomic.Int32
	apiHits       atomic.Int32
	mu            sync.Mutex
	bodies        []string
	onFirstAPI    func() // runs inside the first /api/* request, before it answers
}

func newRotatedServer(t *testing.T) *rotatedServer {
	t.Helper()
	s := &rotatedServer{accept: newAccess}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/cli/token/refresh" {
			s.refreshes.Add(1)
			var in struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if s.refreshStatus != 0 || in.RefreshToken != oldRefresh {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid refresh token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + newAccess + `","refresh_token":"` + newRefresh +
				`","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","email":"u@example.invalid"}`))
			return
		}
		n := s.apiHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		s.mu.Unlock()
		if n == 1 && s.onFirstAPI != nil {
			s.onFirstAPI()
		}
		if s.accept == "" || r.Header.Get("Authorization") != "Bearer "+s.accept {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid or expired token"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// seedSession stores a session for url whose access token has NOT expired
// locally: only the server knows it is dead.
func seedSession(t *testing.T, url, access, refresh string) {
	t.Helper()
	exp := time.Now().Add(50 * time.Minute)
	c := &auth.Credentials{AccessToken: access, RefreshToken: refresh, AccessTokenExpiresAt: exp, ExpiresAt: exp}
	c.BindTo(url)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func stored(t *testing.T, url string) *auth.Credentials {
	t.Helper()
	c, err := auth.LoadCredentialsFor(url)
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	return c
}

func isolatedHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
}

func TestUnauthorizedRenewsOnceAndRetries(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)

	resp, err := New(s.srv.URL).GetWithAuth(context.Background(), "/api/x")
	if err != nil {
		t.Fatalf("GetWithAuth: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after renewing the session", resp.StatusCode)
	}
	if n := s.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
	if n := s.apiHits.Load(); n != 2 {
		t.Fatalf("api hits = %d, want 2 (rejected, then one retry)", n)
	}
	if c := stored(t, s.srv.URL); c.AccessToken != newAccess || c.RefreshToken != newRefresh {
		t.Fatal("the renewed pair was not stored")
	}
}

func TestRetryReplaysTheRequestBody(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)
	c := New(s.srv.URL)

	if _, err := c.PostJSONWithAuth(context.Background(), "/api/x", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("PostJSONWithAuth: %v", err)
	}
	// A plain io.Reader cannot be rewound; PostMultipart must still resend it.
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)
	s.refreshStatus = 0
	payload := "--b\r\nmultipart-payload\r\n--b--\r\n"
	if _, err := c.PostMultipart(context.Background(), "/api/y", io.MultiReader(strings.NewReader(payload)), "multipart/form-data; boundary=b"); err != nil {
		t.Fatalf("PostMultipart: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	want := []string{`{"k":"v"}`, `{"k":"v"}`, payload, payload}
	if len(s.bodies) != len(want) {
		t.Fatalf("api requests = %d, want %d", len(s.bodies), len(want))
	}
	for i := range want {
		if s.bodies[i] != want[i] {
			t.Fatalf("request %d body = %q, want %q", i+1, s.bodies[i], want[i])
		}
	}
}

func TestUnauthorizedAndRejectedRefreshAsksForLogin(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	s.refreshStatus = http.StatusUnauthorized
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)

	_, err := New(s.srv.URL).GetWithAuth(context.Background(), "/api/x")
	if !errors.Is(err, ErrSessionExpired) || !strings.Contains(err.Error(), "taufinity auth login") {
		t.Fatalf("err = %v, want the session-expired login message", err)
	}
	if _, lerr := auth.LoadCredentialsFor(s.srv.URL); lerr == nil {
		t.Fatal("credentials were kept after the refresh token was rejected")
	}
	if n := s.apiHits.Load(); n != 1 {
		t.Fatalf("api hits = %d, want 1: no retry after a failed renewal", n)
	}
}

func TestUnauthorizedWithoutRefreshTokenAsksForLogin(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, "")

	_, err := New(s.srv.URL).GetWithAuth(context.Background(), "/api/x")
	if !errors.Is(err, ErrSessionRejected) || !strings.Contains(err.Error(), "taufinity auth login") {
		t.Fatalf("err = %v, want the login message", err)
	}
	if n := s.refreshes.Load(); n != 0 {
		t.Fatalf("refreshes = %d with no refresh token stored", n)
	}
}

func TestAtMostOneRefreshPerRequestNeverALoop(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	s.accept = "" // refuses every token, the renewed one too
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)

	resp, err := New(s.srv.URL).GetWithAuth(context.Background(), "/api/x")
	if err != nil {
		t.Fatalf("GetWithAuth: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the second 401 returned to the caller", resp.StatusCode)
	}
	if n := s.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", n)
	}
	if n := s.apiHits.Load(); n != 2 {
		t.Fatalf("api hits = %d, want exactly 2", n)
	}
}

func TestExplicitTokenIsNeverRenewed(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)
	c := New(s.srv.URL)
	c.SetAuth("explicit-token")

	resp, err := c.GetWithAuth(context.Background(), "/api/x")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resp = %v, err = %v; want the 401 as is", resp, err)
	}
	if n := s.refreshes.Load(); n != 0 {
		t.Fatalf("refreshes = %d for an explicit token", n)
	}
}

func TestRenewalGoesToTheTokensOwnHost(t *testing.T) {
	isolatedHome(t)
	staging := newRotatedServer(t)
	prod := newRotatedServer(t)
	seedSession(t, staging.srv.URL, oldAccess, oldRefresh)
	seedSession(t, prod.srv.URL, "prod-access", "prod-refresh")

	if _, err := New(staging.srv.URL).GetWithAuth(context.Background(), "/api/x"); err != nil {
		t.Fatalf("GetWithAuth: %v", err)
	}
	if staging.refreshes.Load() != 1 || prod.refreshes.Load() != 0 {
		t.Fatalf("refreshes staging=%d prod=%d, want 1 and 0", staging.refreshes.Load(), prod.refreshes.Load())
	}
	if c := stored(t, prod.srv.URL); c.AccessToken != "prod-access" || c.RefreshToken != "prod-refresh" {
		t.Fatal("renewing one host's session changed another host's credentials")
	}
}

func TestSessionAlreadyRenewedElsewhereIsUsedWithoutRefreshing(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)
	// Another process renews while this request is in flight.
	s.onFirstAPI = func() { seedSession(t, s.srv.URL, newAccess, newRefresh) }

	resp, err := New(s.srv.URL).GetWithAuth(context.Background(), "/api/x")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if n := s.refreshes.Load(); n != 0 {
		t.Fatalf("refreshes = %d; the stored, already renewed token should be used (re-sending a rotated refresh token revokes every session)", n)
	}
}

func TestValidatedTokenRenewsARejectedToken(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)

	tok, err := New(s.srv.URL).ValidatedToken(context.Background())
	if err != nil {
		t.Fatalf("ValidatedToken: %v", err)
	}
	if tok != newAccess {
		t.Fatal("ValidatedToken handed out the token the server refuses")
	}

	s.accept = ""
	seedSession(t, s.srv.URL, oldAccess, oldRefresh)
	if _, err := New(s.srv.URL).ValidatedToken(context.Background()); !errors.Is(err, ErrSessionRejectedByServer) {
		t.Fatalf("err = %v, want ErrSessionRejectedByServer", err)
	}
}

func TestValidatedTokenOfflineReturnsStoredToken(t *testing.T) {
	isolatedHome(t)
	s := newRotatedServer(t)
	url := s.srv.URL
	seedSession(t, url, oldAccess, oldRefresh)
	s.srv.Close() // unreachable

	c := New(url)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	tok, err := c.ValidatedToken(ctx)
	if err != nil || tok != oldAccess {
		t.Fatalf("offline: tok matches stored = %v, err = %v; want the stored token, as before", tok == oldAccess, err)
	}
}

// No message carries a token value.
func TestRenewalErrorsNeverContainTokens(t *testing.T) {
	for _, err := range []error{ErrSessionExpired, ErrSessionRejected, ErrSessionRejectedByServer} {
		for _, v := range []string{oldAccess, oldRefresh, newAccess, newRefresh} {
			if bytes.Contains([]byte(err.Error()), []byte(v)) {
				t.Fatalf("%v contains a token", err)
			}
		}
	}
}
