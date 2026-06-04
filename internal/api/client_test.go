package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

func TestClient_Get(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/test" {
			t.Errorf("expected /api/test, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.Get(context.Background(), "/api/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestClient_PostJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["key"] != "value" {
			t.Errorf("body[key] = %s, want value", body["key"])
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": 123}`))
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.PostJSON(context.Background(), "/api/resource", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("PostJSON failed: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}

func TestClient_WithAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", authHeader, "Bearer test-token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Setup credentials
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	creds := &auth.Credentials{
		AccessToken: "test-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save credentials failed: %v", err)
	}

	client := New(server.URL)
	resp, err := client.GetWithAuth(context.Background(), "/api/protected")
	if err != nil {
		t.Fatalf("GetWithAuth failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestClient_DryRun(t *testing.T) {
	serverCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	client.SetDryRun(true)

	// POST should not be called in dry-run
	_, err := client.PostJSON(context.Background(), "/api/resource", map[string]string{})
	if err != nil {
		t.Fatalf("PostJSON failed: %v", err)
	}
	if serverCalled {
		t.Error("Server should not be called in dry-run mode for POST")
	}

	// GET should still be called (read-only)
	_, err = client.Get(context.Background(), "/api/resource")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !serverCalled {
		t.Error("Server should be called in dry-run mode for GET")
	}
}

func TestRefreshToken_PostsRefreshTokenAndStoresRotation(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	var gotBody map[string]string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		exp := time.Now().Add(time.Hour)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "new-access",
			"refresh_token":     "new-refresh",
			"expires_at":        exp,
			"email":             "u@example.com",
			"organization_name": "Acme",
		})
	}))
	defer server.Close()

	client := New(server.URL)
	creds := &auth.Credentials{AccessToken: "old", RefreshToken: "old-refresh"}
	if err := client.refreshToken(context.Background(), creds); err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if gotPath != "/api/cli/token/refresh" {
		t.Fatalf("wrong path %q", gotPath)
	}
	if gotBody["refresh_token"] != "old-refresh" {
		t.Fatalf("expected refresh_token in body, got %v", gotBody)
	}
	if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" {
		t.Fatalf("tokens not rotated: %+v", creds)
	}
	if creds.OrganizationName != "Acme" || creds.Email != "u@example.com" {
		t.Fatalf("identity not stored: %+v", creds)
	}
}

func TestRefreshToken_NoRefreshTokenErrors(t *testing.T) {
	client := New("http://unused.invalid")
	creds := &auth.Credentials{AccessToken: "old"} // no refresh token
	if err := client.refreshToken(context.Background(), creds); err == nil {
		t.Fatal("expected error when no refresh token is stored")
	}
}

func TestRefreshToken_ServerRejectsReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := New(server.URL)
	creds := &auth.Credentials{RefreshToken: "stale"}
	if err := client.refreshToken(context.Background(), creds); err == nil {
		t.Fatal("expected error when server returns 401")
	}
}

func TestRefreshToken_401ReturnsRejectedSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := New(server.URL)
	creds := &auth.Credentials{RefreshToken: "revoked"}
	err := client.refreshToken(context.Background(), creds)
	if !errors.Is(err, ErrRefreshTokenRejected) {
		t.Fatalf("expected ErrRefreshTokenRejected on 401, got %v", err)
	}
}

func TestRefreshToken_5xxIsNotRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client := New(server.URL)
	creds := &auth.Credentials{RefreshToken: "still-good"}
	err := client.refreshToken(ctx, creds)
	if err == nil {
		t.Fatal("expected error on 503")
	}
	if errors.Is(err, ErrRefreshTokenRejected) {
		t.Fatalf("503 must NOT be classified as a definitive rejection, got %v", err)
	}
}

// TestGetToken_RefreshOn401DeletesCreds: a definitive 401 on refresh clears the
// stored credentials and returns a clear "please log in" error (C2/H1).
func TestGetToken_RefreshOn401DeletesCreds(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	// Near-expiry access token with a refresh token → ShouldRenew triggers.
	creds := &auth.Credentials{
		AccessToken:          "stale",
		RefreshToken:         "revoked",
		AccessTokenExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:            time.Now().Add(time.Minute),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	client := New(server.URL)
	if _, err := client.Token(context.Background()); err == nil {
		t.Fatal("expected error when refresh is rejected")
	}
	if auth.HasCredentials() {
		t.Fatal("credentials should be deleted after a definitive 401")
	}
}

// TestGetToken_RefreshOn503PreservesCreds: a transient 5xx during refresh must
// NOT delete credentials, so a later invocation can retry (H1).
func TestGetToken_RefreshOn503PreservesCreds(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	creds := &auth.Credentials{
		AccessToken:          "stale",
		RefreshToken:         "still-good",
		AccessTokenExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:            time.Now().Add(time.Minute),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Bound the renewal so retry backoff doesn't slow the suite; a caller
	// timeout is a realistic transient failure too.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client := New(server.URL)
	if _, err := client.Token(ctx); err == nil {
		t.Fatal("expected error on 503 refresh")
	}
	if !auth.HasCredentials() {
		t.Fatal("credentials must be preserved on a transient 5xx so a retry is possible")
	}
}

// TestGetToken_RefreshOnNetworkErrorPreservesCreds: a hard network failure must
// also preserve credentials (H1).
func TestGetToken_RefreshOnNetworkErrorPreservesCreds(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Point at a server that's immediately closed → connection refused.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	creds := &auth.Credentials{
		AccessToken:          "stale",
		RefreshToken:         "still-good",
		AccessTokenExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:            time.Now().Add(time.Minute),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	client := New(url)
	if _, err := client.Token(ctx); err == nil {
		t.Fatal("expected error on network failure")
	}
	if !auth.HasCredentials() {
		t.Fatal("credentials must be preserved on a network error")
	}
}

// TestToken_RenewsNearExpiry: the public Token() must trigger a refresh when the
// access token is near expiry, returning the freshly-rotated token (C2).
func TestToken_RenewsNearExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/token/refresh" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		refreshCalls.Add(1)
		exp := time.Now().Add(time.Hour)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"expires_at":    exp,
		})
	}))
	defer server.Close()

	// Access token expires in 1 minute → within the 5m RenewBuffer.
	creds := &auth.Credentials{
		AccessToken:          "near-expiry",
		RefreshToken:         "valid-refresh",
		AccessTokenExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:            time.Now().Add(time.Minute),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	client := New(server.URL)
	tok, err := client.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-access" {
		t.Fatalf("Token() = %q, want fresh-access (should have renewed)", tok)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", refreshCalls.Load())
	}

	// The rotated pair must be persisted for the next process.
	reloaded, err := auth.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if reloaded.AccessToken != "fresh-access" || reloaded.RefreshToken != "fresh-refresh" {
		t.Fatalf("rotated pair not persisted: %+v", reloaded)
	}
}

// TestToken_NoRenewalWhenFresh: a token with comfortable headroom must NOT
// trigger a refresh (single code path, no needless server calls).
func TestToken_NoRenewalWhenFresh(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	creds := &auth.Credentials{
		AccessToken:          "fresh-enough",
		RefreshToken:         "valid-refresh",
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
		ExpiresAt:            time.Now().Add(time.Hour),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	client := New(server.URL)
	tok, err := client.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-enough" {
		t.Fatalf("Token() = %q, want existing token", tok)
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("expected no refresh call for a fresh token, got %d", refreshCalls.Load())
	}
}

func TestRevokeRefreshToken_PostsBodyUnauthenticated(t *testing.T) {
	var gotBody map[string]string
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	if err := client.RevokeRefreshToken("rt-123"); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	if gotPath != "/api/cli/token/revoke" {
		t.Fatalf("wrong path %q", gotPath)
	}
	if gotBody["refresh_token"] != "rt-123" {
		t.Fatalf("expected refresh_token in body, got %v", gotBody)
	}
	if gotAuth != "" {
		t.Fatalf("revoke should be unauthenticated, got auth %q", gotAuth)
	}
}

func TestRevokeAllRefreshTokens_AuthenticatedPost(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// SetAuth → getToken() short-circuits, no creds file needed.
	client := New(server.URL)
	client.SetAuth("tkn")
	if err := client.RevokeAllRefreshTokens(); err != nil {
		t.Fatalf("RevokeAllRefreshTokens: %v", err)
	}
	if gotAuth != "Bearer tkn" {
		t.Fatalf("expected Bearer auth, got %q", gotAuth)
	}
	if gotPath != "/api/cli/token/revoke-all" {
		t.Fatalf("wrong path %q", gotPath)
	}
}

func TestRevokeAllRefreshTokens_ServerErrorReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := New(server.URL)
	client.SetAuth("tkn")
	if err := client.RevokeAllRefreshTokens(); err == nil {
		t.Fatal("expected error on non-200 revoke-all")
	}
}

func TestClient_Retry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.Get(context.Background(), "/api/flaky")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if attempts < 3 {
		t.Errorf("Expected at least 3 attempts, got %d", attempts)
	}
}

func TestClient_OrgHeader(t *testing.T) {
	type capture struct {
		method string
		path   string
		org    string
	}
	var got capture

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = capture{
			method: r.Method,
			path:   r.URL.Path,
			org:    r.Header.Get("X-Organization-ID"),
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// Stub auth credentials so *WithAuth methods don't bail on missing token.
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)
	creds := &auth.Credentials{
		AccessToken: "test-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save credentials failed: %v", err)
	}

	t.Run("SetOrg sets header on GetWithAuth", func(t *testing.T) {
		got = capture{}
		client := New(server.URL)
		client.SetOrg("12")
		if _, err := client.GetWithAuth(context.Background(), "/api/x"); err != nil {
			t.Fatalf("GetWithAuth failed: %v", err)
		}
		if got.org != "12" {
			t.Errorf("X-Organization-ID = %q, want %q", got.org, "12")
		}
	})

	t.Run("SetOrg sets header on PostJSONWithAuth", func(t *testing.T) {
		got = capture{}
		client := New(server.URL)
		client.SetOrg("12")
		if _, err := client.PostJSONWithAuth(context.Background(), "/api/x", map[string]string{"k": "v"}); err != nil {
			t.Fatalf("PostJSONWithAuth failed: %v", err)
		}
		if got.org != "12" {
			t.Errorf("X-Organization-ID = %q, want %q", got.org, "12")
		}
	})

	t.Run("SetOrg sets header on DeleteWithAuth", func(t *testing.T) {
		got = capture{}
		client := New(server.URL)
		client.SetOrg("12")
		if _, err := client.DeleteWithAuth(context.Background(), "/api/x/1"); err != nil {
			t.Fatalf("DeleteWithAuth failed: %v", err)
		}
		if got.org != "12" {
			t.Errorf("X-Organization-ID = %q, want %q", got.org, "12")
		}
	})

	t.Run("no SetOrg call leaves header unset", func(t *testing.T) {
		got = capture{}
		client := New(server.URL)
		if _, err := client.GetWithAuth(context.Background(), "/api/x"); err != nil {
			t.Fatalf("GetWithAuth failed: %v", err)
		}
		if got.org != "" {
			t.Errorf("X-Organization-ID = %q, want empty", got.org)
		}
	})
}
