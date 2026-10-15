package commands

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Studio instance can sit behind Cloudflare Access, which gates every request
// before it reaches the app. Provision must present a CF-Access service token to
// get through — on reads and writes alike, because the diff GETs remote state and
// would 403 before it computed a change if only writes carried the header.
//
// With the environment set, the token goes to the configured host (here an
// httptest server); the gcloud fallback is staging-only, see internal/cfaccess. These tests pin: it rides both
// paths when set; nothing is sent when unset; and it is never one-sided.
func TestCFAccess_HeadersRideReadAndWritePaths(t *testing.T) {
	var readID, readSecret, writeID, writeSecret string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			readID = r.Header.Get("CF-Access-Client-Id")
			readSecret = r.Header.Get("CF-Access-Client-Secret")
		} else {
			writeID = r.Header.Get("CF-Access-Client-Id")
			writeSecret = r.Header.Get("CF-Access-Client-Secret")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("CF_ACCESS_CLIENT_ID", "svc-id")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "svc-secret")
	c := newProvisionClient(srv.URL, "tok", false)

	if _, _, err := c.get("/anything"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, _, err := c.put("/anything", []byte(`{}`)); err != nil {
		t.Fatalf("put: %v", err)
	}

	if readID != "svc-id" || readSecret != "svc-secret" {
		t.Errorf("read path missing CF-Access token: id=%q secret=%q — a diff GET would 403 behind Access", readID, readSecret)
	}
	if writeID != "svc-id" || writeSecret != "svc-secret" {
		t.Errorf("write path missing CF-Access token: id=%q secret=%q", writeID, writeSecret)
	}
}

func TestCFAccess_NothingSentWhenUnset(t *testing.T) {
	var sawID, sawSecret bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != "" {
			sawID = true
		}
		if r.Header.Get("CF-Access-Client-Secret") != "" {
			sawSecret = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
	c := newProvisionClient(srv.URL, "tok", false)

	if _, _, err := c.get("/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, _, err := c.put("/x", []byte(`{}`)); err != nil {
		t.Fatalf("put: %v", err)
	}

	if sawID || sawSecret {
		t.Errorf("CF-Access headers were sent while unconfigured (id=%v secret=%v) — a non-Access host must be reached with no CF-Access headers at all", sawID, sawSecret)
	}
}

// A half-configured token is worse than none: it identifies nothing to Cloudflare
// and could confuse an operator into thinking Access is handled. Send both or
// neither.
func TestCFAccess_HalfConfiguredSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		id, secret string
	}{
		{"id only", "svc-id", ""},
		{"secret only", "", "svc-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("CF-Access-Client-Id") != "" || r.Header.Get("CF-Access-Client-Secret") != "" {
					seen = true
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			t.Setenv("CF_ACCESS_CLIENT_ID", tc.id)
			t.Setenv("CF_ACCESS_CLIENT_SECRET", tc.secret)
			c := newProvisionClient(srv.URL, "tok", false)

			if _, _, err := c.get("/x"); err != nil {
				t.Fatalf("get: %v", err)
			}
			if seen {
				t.Errorf("%s: a half-configured token must send no CF-Access header", tc.name)
			}
		})
	}
}
