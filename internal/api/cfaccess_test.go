package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/cfaccess"
)

// Fake values only.
const (
	fakeCFID     = "fake-client-id"
	fakeCFSecret = "fake-client-secret"
)

// routeAll sends every request to the test server while the cfaccess
// transport still sees the real staging/prod URL.
type routeAll struct{ target *url.URL }

func (r routeAll) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = r.target.Scheme, r.target.Host, r.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

type cfCapture struct {
	mu    sync.Mutex
	paths map[string][2]string // path -> (id, secret)
	hits  atomic.Int32
}

func (c *cfCapture) get(path string) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.paths[path]
	return v[0], v[1]
}

// withFakeStaging routes all HTTP to handler, swaps gcloud for a counting fake
// and clears the env vars, for the duration of the test.
func withFakeStaging(t *testing.T, handler http.HandlerFunc) (*cfCapture, *atomic.Int32) {
	t.Helper()
	t.Setenv(cfaccess.EnvClientID, "")
	t.Setenv(cfaccess.EnvClientSecret, "")
	capt := &cfCapture{paths: map[string][2]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.hits.Add(1)
		capt.mu.Lock()
		capt.paths[r.URL.Path] = [2]string{r.Header.Get(cfaccess.HeaderClientID), r.Header.Get(cfaccess.HeaderClientSecret)}
		capt.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)

	var fetches atomic.Int32
	res := cfaccess.NewResolver()
	res.Fetch = func(_ context.Context, name string) (string, error) {
		fetches.Add(1)
		if name == cfaccess.SecretClientID {
			return fakeCFID, nil
		}
		return fakeCFSecret, nil
	}
	t.Cleanup(cfaccess.SetForTest(res, routeAll{target}))
	return capt, &fetches
}

func jsonOK(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// The device-code login flow is unauthenticated (no token yet): PostJSON for
// the code and Get for the poll. Both must carry the staging token.
func TestDeviceCodeFlowCarriesStagingToken(t *testing.T) {
	capt, fetches := withFakeStaging(t, jsonOK(`{"status":"pending"}`))
	c := New("https://" + cfaccess.StagingHost)

	if _, err := c.PostJSON(context.Background(), "/api/cli/device/code", map[string]string{"client_id": "taufinity-cli"}); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if _, err := c.Get(context.Background(), "/api/cli/device/code/abc/status"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, p := range []string{"/api/cli/device/code", "/api/cli/device/code/abc/status"} {
		if id, sec := capt.get(p); id != fakeCFID || sec != fakeCFSecret {
			t.Errorf("%s: staging token missing (id set=%v secret set=%v)", p, id != "", sec != "")
		}
	}
	if n := fetches.Load(); n != 2 {
		t.Errorf("gcloud fetches = %d, want 2 for the whole process", n)
	}
}

// Refresh and revoke used to skip the CF-Access headers entirely, so a staging
// session could never renew. They go through the same transport now.
func TestRefreshAndRevokeCarryStagingToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	capt, _ := withFakeStaging(t, jsonOK(`{"access_token":"new-a","refresh_token":"new-r","expires_at":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`))
	creds := &auth.Credentials{AccessToken: "old", RefreshToken: "r1", AccessTokenExpiresAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(-time.Minute)}
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}
	c := New("https://" + cfaccess.StagingHost)
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if err := c.RevokeRefreshToken("new-r"); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	for _, p := range []string{"/api/cli/token/refresh", "/api/cli/token/revoke"} {
		if id, _ := capt.get(p); id != fakeCFID {
			t.Errorf("%s went out without the staging token", p)
		}
	}
}

func TestProdNeverGetsStagingToken(t *testing.T) {
	capt, fetches := withFakeStaging(t, jsonOK(`{}`))
	c := New("https://studio.taufinity.io")
	if _, err := c.PostJSON(context.Background(), "/api/cli/device/code", nil); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if id, sec := capt.get("/api/cli/device/code"); id != "" || sec != "" {
		t.Fatal("production received CF-Access headers")
	}
	if n := fetches.Load(); n != 0 {
		t.Fatalf("gcloud ran %d times for a production request", n)
	}
}

// The symptom that started this: a 302 to the Access login surfaced as
// "decode response: invalid character '<'". It must be the actionable error,
// at once, without the retry backoff.
func TestAccessRedirectIsClearErrorNotDecodeError(t *testing.T) {
	capt, _ := withFakeStaging(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://taufinity.cloudflareaccess.com/cdn-cgi/access/login/x", http.StatusFound)
	})
	c := New("https://" + cfaccess.StagingHost)
	start := time.Now()
	_, err := c.PostJSON(context.Background(), "/api/cli/device/code", nil)
	if cfaccess.AsBlocked(err) == nil {
		t.Fatalf("want a Cloudflare Access error, got %v", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("still the JSON decode error: %v", err)
	}
	if n := capt.hits.Load(); n != 1 {
		t.Fatalf("server hits = %d, want 1: an Access block must not be retried", n)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("an Access block went through retry backoff")
	}
}
