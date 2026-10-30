package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

// After a Studio signing-key rotation the stored access token is dead before
// its local expiry. `auth status` and `auth token` must renew once instead of
// reporting the session invalid or handing out the dead token.

// Fake token values only.
const (
	rotOldAccess  = "rot-old-access"
	rotOldRefresh = "rot-old-refresh"
	rotNewAccess  = "rot-new-access"
	rotNewRefresh = "rot-new-refresh"
)

func rotatedStudio(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/cli/token/refresh":
			refreshes.Add(1)
			var in struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.RefreshToken != rotOldRefresh {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + rotNewAccess + `","refresh_token":"` + rotNewRefresh +
				`","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","email":"u@example.invalid"}`))
		case "/api/cli/token/validate":
			if r.Header.Get("Authorization") != "Bearer "+rotNewAccess {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"valid":false,"error":"invalid or expired token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"valid":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &refreshes
}

func seedRotatedSession(t *testing.T, apiURL string) {
	t.Helper()
	exp := time.Now().Add(50 * time.Minute) // not expired locally
	c := &auth.Credentials{AccessToken: rotOldAccess, RefreshToken: rotOldRefresh, AccessTokenExpiresAt: exp, ExpiresAt: exp, Email: "u@example.invalid"}
	c.BindTo(apiURL)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		rootCmd.SetArgs(args)
		err = rootCmd.Execute()
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	return out, err
}

func TestAuthStatusRenewsAfterKeyRotation(t *testing.T) {
	resetGlobals(t)
	t.Setenv("HOME", t.TempDir())
	srv, refreshes := rotatedStudio(t)
	seedRotatedSession(t, srv.URL)

	out, err := runRoot(t, "--api-url", srv.URL, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v", err)
	}
	if !strings.Contains(out, "Status: Authenticated") {
		t.Fatalf("auth status after a key rotation = %q, want Authenticated after one renewal", out)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
	if strings.Contains(out, rotNewAccess) || strings.Contains(out, rotOldAccess) {
		t.Fatal("auth status printed a token")
	}
}

func TestAuthTokenHandsOutTheRenewedToken(t *testing.T) {
	resetGlobals(t)
	t.Setenv("HOME", t.TempDir())
	srv, refreshes := rotatedStudio(t)
	seedRotatedSession(t, srv.URL)

	out, err := runRoot(t, "--api-url", srv.URL, "auth", "token")
	if err != nil {
		t.Fatalf("auth token: %v", err)
	}
	if strings.TrimSpace(out) != rotNewAccess {
		t.Fatalf("auth token printed the stale token (or something else), want the renewed one")
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
}

func TestAuthTokenFailsWhenTheRefreshIsRejected(t *testing.T) {
	resetGlobals(t)
	t.Setenv("HOME", t.TempDir())
	srv, _ := rotatedStudio(t)
	exp := time.Now().Add(50 * time.Minute)
	c := &auth.Credentials{AccessToken: rotOldAccess, RefreshToken: "revoked-refresh", AccessTokenExpiresAt: exp, ExpiresAt: exp}
	c.BindTo(srv.URL)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, "--api-url", srv.URL, "auth", "token")
	if err == nil || !strings.Contains(err.Error(), "taufinity auth login") {
		t.Fatalf("err = %v, want the login message", err)
	}
	if strings.Contains(out, rotOldAccess) {
		t.Fatal("auth token printed a token the server refuses and that cannot be renewed")
	}
}

// The MCP bridge renews once on an upstream 401 and retries the request once.
func TestStdioBridge_RenewsOnceAfterUpstream401(t *testing.T) {
	var hits, renewals atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+rotNewAccess {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(in.ID) + `,"result":{"ok":true}}`))
	}))
	defer upstream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in, out, _, done := runBridgeAsyncWithConfig(t, ctx, StdioBridgeConfig{
		UpstreamURL: upstream.URL + "/mcp",
		TokenSource: func(context.Context) (string, time.Time, error) {
			return rotOldAccess, time.Now().Add(50 * time.Minute), nil
		},
		RenewAfterReject: func(_ context.Context, rejected string) (string, time.Time, error) {
			renewals.Add(1)
			if rejected != rotOldAccess {
				t.Errorf("renew got the wrong rejected token")
			}
			return rotNewAccess, time.Now().Add(time.Hour), nil
		},
		Timeout: 5 * time.Second,
	})
	if _, err := in.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	frame := readNextFrame(t, out, 5*time.Second)
	_ = in.Close()
	<-done

	if !strings.Contains(string(frame), `"result"`) {
		t.Fatalf("response = %s, want a result after the renewal", frame)
	}
	if n := renewals.Load(); n != 1 {
		t.Fatalf("renewals = %d, want 1", n)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("upstream hits = %d, want 2", n)
	}
}

// An access token that has expired locally but has a refresh token is renewed,
// not reported "Expired".
func TestAuthStatusRenewsALocallyExpiredSession(t *testing.T) {
	resetGlobals(t)
	t.Setenv("HOME", t.TempDir())
	srv, refreshes := rotatedStudio(t)
	past := time.Now().Add(-time.Hour)
	c := &auth.Credentials{AccessToken: rotOldAccess, RefreshToken: rotOldRefresh, AccessTokenExpiresAt: past, ExpiresAt: past, Email: "u@example.invalid"}
	c.BindTo(srv.URL)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "--api-url", srv.URL, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v", err)
	}
	if !strings.Contains(out, "Status: Authenticated") || refreshes.Load() != 1 {
		t.Fatalf("auth status = %q (refreshes %d), want Authenticated after one renewal", out, refreshes.Load())
	}
}
