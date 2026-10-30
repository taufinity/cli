package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

func seedProdSession(t *testing.T) {
	t.Helper()
	c := &auth.Credentials{AccessToken: "prod-a", RefreshToken: "prod-r",
		AccessTokenExpiresAt: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
	c.BindTo(DefaultAPIURL)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func assertProdSessionIntact(t *testing.T) {
	t.Helper()
	got, err := auth.LoadCredentialsFor(DefaultAPIURL)
	if err != nil {
		t.Fatalf("production session lost: %v", err)
	}
	if got.RefreshToken != "prod-r" {
		t.Fatal("production session was replaced")
	}
}

// A login that fails before approval (here the server refuses the device
// code; on staging it used to be Cloudflare Access) used to have deleted the
// existing session already.
func TestAuthLogin_FailureKeepsExistingSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedProdSession(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	// Requests go to the test server; the session in play is production's file.
	prev := flagAPIURL
	flagAPIURL = srv.URL
	t.Cleanup(func() { flagAPIURL = prev })
	bindCredsHost(t, DefaultAPIURL)

	if err := runAuthLogin(nil, nil); err == nil {
		t.Fatal("expected the login to fail")
	}
	assertProdSessionIntact(t)
}

// A completed login against another host (staging) is saved for that host
// only; the production session is untouched.
func TestAuthLogin_OtherHostDoesNotReplaceProduction(t *testing.T) {
	if testing.Short() {
		t.Skip("waits one device-code poll interval")
	}
	t.Setenv("HOME", t.TempDir())
	seedProdSession(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/cli/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc", "user_code": "UC", "verification_uri": "http://x/device",
				"expires_in": 30, "interval": 1,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "approved", "access_token": "stg-a", "refresh_token": "stg-r",
				"expires_at": time.Now().Add(time.Hour), "email": "e@example.com",
			})
		}
	}))
	defer srv.Close()
	withTestAPIURL(t, srv)
	prevQuiet := flagQuiet
	flagQuiet = true // no browser
	t.Cleanup(func() { flagQuiet = prevQuiet })

	if err := runAuthLogin(nil, nil); err != nil {
		t.Fatalf("login: %v", err)
	}
	got, err := auth.LoadCredentialsFor(srv.URL)
	if err != nil || got.RefreshToken != "stg-r" {
		t.Fatalf("other-host session not saved under its own host: %v", err)
	}
	assertProdSessionIntact(t)
}
