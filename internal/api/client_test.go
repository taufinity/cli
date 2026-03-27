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
