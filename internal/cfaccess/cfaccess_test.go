package cfaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Fake values only. Nothing in this file ever touches the real secrets.
const (
	fakeID     = "fake-client-id"
	fakeSecret = "fake-client-secret"
	envID      = "env-client-id"
	envSecret  = "env-client-secret"
)

// routeTo sends every request to target while the Transport under test still
// sees the original URL (and so the original host).
type routeTo struct{ target *url.URL }

func (r routeTo) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = r.target.Scheme
	out.URL.Host = r.target.Host
	out.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

type seen struct {
	mu   sync.Mutex
	id   []string
	sec  []string
	hits int
}

func (s *seen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = append(s.id, r.Header.Get(HeaderClientID))
	s.sec = append(s.sec, r.Header.Get(HeaderClientSecret))
	s.hits++
}

func (s *seen) last() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.id) == 0 {
		return "", ""
	}
	return s.id[len(s.id)-1], s.sec[len(s.sec)-1]
}

// fixture is a JSON server, a resolver with a counting fake fetch and no env,
// and an http.Client routed through the Transport under test.
type fixture struct {
	srv     *httptest.Server
	seen    *seen
	fetches atomic.Int32
	env     map[string]string
	res     *Resolver
	client  *http.Client
}

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{seen: &seen{}, env: map[string]string{}}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen.record(r)
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	target, _ := url.Parse(f.srv.URL)
	f.res = &Resolver{
		Getenv: func(k string) string { return f.env[k] },
		Fetch: func(_ context.Context, _, name string) (string, error) {
			f.fetches.Add(1)
			switch name {
			case SecretClientID:
				return fakeID + "\n", nil // gcloud prints a trailing newline
			case SecretClientSecret:
				return fakeSecret + "\n", nil
			}
			return "", fmt.Errorf("unexpected secret %q", name)
		},
	}
	f.client = &http.Client{Transport: &Transport{Base: routeTo{target}, Resolver: f.res}}
	return f
}

func (f *fixture) get(t *testing.T, rawURL string) (*http.Response, error) {
	t.Helper()
	resp, err := f.client.Get(rawURL)
	if resp != nil {
		resp.Body.Close()
	}
	return resp, err
}

func TestStagingHostGetsGcloudToken(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.get(t, "https://"+StagingHost+"/api/cli/device/code"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if id, sec := f.seen.last(); id != fakeID || sec != fakeSecret {
		t.Fatalf("staging request did not carry the fetched token (id set=%v, secret set=%v)", id != "", sec != "")
	}
}

func TestGcloudFetchedOncePerProcess(t *testing.T) {
	f := newFixture(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.get(t, "https://"+StagingHost+"/api/x")
		}()
	}
	wg.Wait()
	if n := f.fetches.Load(); n != 2 {
		t.Fatalf("gcloud fetches = %d, want exactly 2 (one per secret) for the whole process", n)
	}
}

func TestNoTokenForProdOrOtherHosts(t *testing.T) {
	for _, host := range []string{
		"studio.taufinity.io",                  // production
		"my.taufinity.io",                      // portal
		"example.com",                          // anything else
		"staging-studio.taufinity.io.evil.com", // suffix trick
		"evil-staging-studio.taufinity.io",     // prefix trick
	} {
		t.Run(host, func(t *testing.T) {
			f := newFixture(t, nil)
			if _, err := f.get(t, "https://"+host+"/api/x"); err != nil {
				t.Fatalf("get: %v", err)
			}
			if id, sec := f.seen.last(); id != "" || sec != "" {
				t.Fatalf("%s received CF-Access headers; the staging token must never leave staging", host)
			}
			if n := f.fetches.Load(); n != 0 {
				t.Fatalf("gcloud was called %d times for %s; it must only run for staging", n, host)
			}
		})
	}
}

func TestNoGcloudTokenOverPlainHTTP(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.get(t, "http://"+StagingHost+"/api/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if id, _ := f.seen.last(); id != "" {
		t.Fatal("the gcloud token went out over plain http")
	}
}

func TestEnvTakesPrecedenceOverGcloud(t *testing.T) {
	f := newFixture(t, nil)
	f.env[EnvClientID] = envID
	f.env[EnvClientSecret] = envSecret
	if _, err := f.get(t, "https://"+StagingHost+"/api/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	id, sec := f.seen.last()
	if id != envID || sec != envSecret {
		t.Fatalf("env token not used on staging (got id from env=%v, secret from env=%v)", id == envID, sec == envSecret)
	}
	if n := f.fetches.Load(); n != 0 {
		t.Fatalf("gcloud called %d times although the env token was set", n)
	}
}

func TestHalfSetEnvFallsBackToGcloudOnStaging(t *testing.T) {
	f := newFixture(t, nil)
	f.env[EnvClientID] = envID // secret missing: the pair is unusable
	if _, err := f.get(t, "https://"+StagingHost+"/api/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if id, _ := f.seen.last(); id != fakeID {
		t.Fatal("a half-set env pair must not be sent; staging should fall back to gcloud")
	}
}

func TestExplicitHeaderIsKept(t *testing.T) {
	f := newFixture(t, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://"+StagingHost+"/api/x", nil)
	req.Header.Set(HeaderClientID, "explicit-id")
	req.Header.Set(HeaderClientSecret, "explicit-secret")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if id, _ := f.seen.last(); id != "explicit-id" {
		t.Fatal("a token the caller set explicitly was overwritten")
	}
	if n := f.fetches.Load(); n != 0 {
		t.Fatalf("gcloud called %d times although the request already had a token", n)
	}
}

func TestCallerRequestNotMutated(t *testing.T) {
	f := newFixture(t, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://"+StagingHost+"/api/x", nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if req.Header.Get(HeaderClientID) != "" || req.Header.Get(HeaderClientSecret) != "" {
		t.Fatal("the transport wrote the token into the caller's request; it would follow redirects to other hosts")
	}
}

const wantNoTokenMsg = `staging-studio.taufinity.io is behind Cloudflare Access and no service token was available: `

func accessRedirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", "https://taufinity.cloudflareaccess.com/cdn-cgi/access/login/staging-studio.taufinity.io?redirect_url=%2Fapi%2Fcli%2Fdevice%2Fcode")
	w.WriteHeader(http.StatusFound)
}

func TestAccessRedirectGivesClearError(t *testing.T) {
	f := newFixture(t, accessRedirect)
	f.res.Fetch = func(context.Context, string, string) (string, error) {
		f.fetches.Add(1)
		return "", errors.New("gcloud is not installed or not on PATH")
	}
	_, err := f.get(t, "https://"+StagingHost+"/api/cli/device/code")
	be := AsBlocked(err)
	if be == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatal("errors.Is(err, ErrBlocked) = false")
	}
	if !strings.HasPrefix(be.Error(), wantNoTokenMsg) {
		t.Fatalf("message = %q\nwant prefix %q", be.Error(), wantNoTokenMsg)
	}
	if !strings.Contains(be.Error(), "gcloud is not installed") {
		t.Fatalf("message should carry the gcloud reason: %q", be.Error())
	}
	if f.seen.hits != 1 {
		t.Fatalf("server hits = %d; the redirect to the Access login must not be followed", f.seen.hits)
	}
}

func TestAccessRedirectFromAnyHostIsBlocked(t *testing.T) {
	f := newFixture(t, accessRedirect)
	_, err := f.get(t, "https://studio.taufinity.io/api/x")
	if AsBlocked(err) == nil {
		t.Fatalf("a redirect to *.cloudflareaccess.com must be reported as an Access block, got %v", err)
	}
}

func TestStagingHTMLWhereJSONExpectedIsBlocked(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<!DOCTYPE html><html>Forbidden</html>"))
	})
	_, err := f.get(t, "https://"+StagingHost+"/api/cli/token/validate")
	be := AsBlocked(err)
	if be == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	if !be.TokenSent {
		t.Fatal("the fetched token was sent; TokenSent should be true")
	}
	if !strings.Contains(be.Error(), "was refused (rotated or revoked?)") {
		t.Fatalf("a rejected token needs its own message, got %q", be.Error())
	}
}

func TestHTMLOutsideStagingAPIIsPassedThrough(t *testing.T) {
	html := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}
	for _, u := range []string{
		"https://studio.taufinity.io/api/x", // prod: not ours to judge
		"https://" + StagingHost + "/",      // not an API path
	} {
		f := newFixture(t, html)
		if _, err := f.get(t, u); err != nil {
			t.Fatalf("%s: unexpected error %v", u, err)
		}
	}
}

func TestFetchFailureSendsNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.res.Fetch = func(_ context.Context, _, name string) (string, error) {
		f.fetches.Add(1)
		if name == SecretClientSecret {
			return "", errors.New("permission denied")
		}
		return fakeID, nil
	}
	if _, err := f.get(t, "https://"+StagingHost+"/api/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if id, sec := f.seen.last(); id != "" || sec != "" {
		t.Fatal("half a token was sent after one secret failed to load")
	}
	if f.res.FetchError(StagingHost) == nil {
		t.Fatal("FetchError should report the failure")
	}
}

func TestTokenNeverPrints(t *testing.T) {
	tok := Token{ID: fakeID, Secret: fakeSecret}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok)} {
		if strings.Contains(s, fakeID) || strings.Contains(s, fakeSecret) {
			t.Fatalf("formatting a Token leaked a value: %q", s)
		}
	}
}

func TestIsAutoHost(t *testing.T) {
	if !IsAutoHost("staging-studio.taufinity.io") || !IsAutoHost("STAGING-STUDIO.taufinity.io") {
		t.Fatal("staging must be an auto host")
	}
	if IsAutoHost("studio.taufinity.io") {
		t.Fatal("production must never be an auto host")
	}
}
