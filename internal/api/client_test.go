package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	if err := client.refreshToken(creds); err != nil {
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
	if err := client.refreshToken(creds); err == nil {
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
	if err := client.refreshToken(creds); err == nil {
		t.Fatal("expected error when server returns 401")
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
