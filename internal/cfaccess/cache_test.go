package cfaccess

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Fake values only. Nothing in this file ever touches the real secrets.
const (
	cachedID     = "cached-client-id"
	cachedSecret = "cached-client-secret"
	freshID      = "fresh-client-id"
	freshSecret  = "fresh-client-secret"
)

// cacheFixture is a resolver with a temp cache dir and a counting fake gcloud
// that returns (id, secret) as currently set.
type cacheFixture struct {
	dir     string
	env     map[string]string
	fetches atomic.Int32
	id      string
	secret  string
	res     *Resolver
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	f := &cacheFixture{dir: filepath.Join(t.TempDir(), "taufinity"), env: map[string]string{}, id: freshID, secret: freshSecret}
	f.res = &Resolver{
		Getenv:   func(k string) string { return f.env[k] },
		CacheDir: func() string { return f.dir },
		Fetch: func(_ context.Context, project, name string) (string, error) {
			f.fetches.Add(1)
			if project != GCPProject {
				return "", errors.New("wrong project " + project)
			}
			switch name {
			case SecretClientID:
				return f.id + "\n", nil
			case SecretClientSecret:
				return f.secret + "\n", nil
			}
			return "", errors.New("unexpected secret " + name)
		},
	}
	return f
}

func (f *cacheFixture) seedCache(t *testing.T, id, secret string) {
	t.Helper()
	if err := f.res.writeCache(StagingHost, Token{ID: id, Secret: secret}); err != nil {
		t.Fatal(err)
	}
}

func TestResolverOrder_EnvBeatsCacheBeatsGcloud(t *testing.T) {
	// gcloud only: fetched, then cached.
	f := newCacheFixture(t)
	tok, src := f.res.For(StagingHost)
	if src != SourceGcloud || tok.ID != freshID {
		t.Fatalf("empty cache: source = %v, want gcloud", src)
	}
	if n := f.fetches.Load(); n != 2 {
		t.Fatalf("gcloud fetches = %d, want 2", n)
	}

	// cache beats gcloud: a new process (resolver) reads the file, no gcloud.
	f2 := newCacheFixture(t)
	f2.seedCache(t, cachedID, cachedSecret)
	tok, src = f2.res.For(StagingHost)
	if src != SourceCache || tok.ID != cachedID || tok.Secret != cachedSecret {
		t.Fatalf("with cache: source = %v, want cache", src)
	}
	if n := f2.fetches.Load(); n != 0 {
		t.Fatalf("gcloud fetches = %d with a cache file, want 0", n)
	}

	// env beats cache.
	f3 := newCacheFixture(t)
	f3.seedCache(t, cachedID, cachedSecret)
	f3.env[EnvClientID], f3.env[EnvClientSecret] = envID, envSecret
	tok, src = f3.res.For(StagingHost)
	if src != SourceEnv || tok.ID != envID {
		t.Fatalf("with env and cache: source = %v, want env", src)
	}
	if n := f3.fetches.Load(); n != 0 {
		t.Fatalf("gcloud fetches = %d with env set, want 0", n)
	}
}

func TestGcloudTokenIsCachedForTheNextProcess(t *testing.T) {
	f := newCacheFixture(t)
	f.res.For(StagingHost)

	next := &Resolver{Getenv: f.res.Getenv, CacheDir: f.res.CacheDir, Fetch: func(context.Context, string, string) (string, error) {
		t.Fatal("the next process called gcloud although the token was cached")
		return "", nil
	}}
	tok, src := next.For(StagingHost)
	if src != SourceCache || tok.ID != freshID || tok.Secret != freshSecret {
		t.Fatalf("next process: source = %v, want cache with the fetched token", src)
	}
}

func TestCacheFileIs0600InA0700Dir(t *testing.T) {
	f := newCacheFixture(t)
	f.res.For(StagingHost)
	info, err := os.Stat(f.res.CachePath(StagingHost))
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cache file mode = %o, want 600", perm)
	}
	dinfo, err := os.Stat(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("cache dir mode = %o, want 700", perm)
	}
}

func TestCacheWriteIsAtomic(t *testing.T) {
	f := newCacheFixture(t)
	f.seedCache(t, cachedID, cachedSecret)
	before, err := os.ReadFile(f.res.CachePath(StagingHost))
	if err != nil {
		t.Fatal(err)
	}

	// No temp file is left beside the target.
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	// A write that cannot complete leaves the old file whole: the new content
	// goes to a temp file first, never into the target in place.
	if err := os.Chmod(f.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; cannot provoke a failed write")
	}
	if err := f.res.writeCache(StagingHost, Token{ID: freshID, Secret: freshSecret}); err == nil {
		t.Fatal("write into a read-only directory succeeded: it wrote the target in place instead of temp file + rename")
	}
	after, err := os.ReadFile(f.res.CachePath(StagingHost))
	if err != nil {
		t.Fatalf("old cache gone after a failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("a failed write changed the cache file in place")
	}
}

func TestLooseCachePermsAreNotTrusted(t *testing.T) {
	f := newCacheFixture(t)
	f.seedCache(t, cachedID, cachedSecret)
	if err := os.Chmod(f.res.CachePath(StagingHost), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, src := f.res.For(StagingHost); src != SourceGcloud {
		t.Fatalf("a world-readable cache was used (source %v); it must be re-read from gcloud", src)
	}
}

func TestUnknownHostsNeverReadCacheOrCallGcloud(t *testing.T) {
	f := newCacheFixture(t)
	f.seedCache(t, cachedID, cachedSecret)
	// Even a cache file named for production must not be used.
	if err := f.res.writeCache("studio.taufinity.io", Token{ID: cachedID, Secret: cachedSecret}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"studio.taufinity.io", "my.taufinity.io", "localhost"} {
		if tok, src := f.res.For(host); src != SourceNone || tok.valid() {
			t.Fatalf("%s got a token from %v", host, src)
		}
	}
	if n := f.fetches.Load(); n != 0 {
		t.Fatalf("gcloud called %d times for unknown hosts", n)
	}
}

// accessGate answers JSON only for the accepted client id; anything else is
// sent to the Access login, the way Cloudflare does it.
type accessGate struct {
	accept string
	hits   atomic.Int32
	bodies []string
}

func (g *accessGate) RoundTrip(req *http.Request) (*http.Response, error) {
	g.hits.Add(1)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		g.bodies = append(g.bodies, string(b))
	}
	h := http.Header{}
	if req.Header.Get(HeaderClientID) == g.accept && g.accept != "" {
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: req}, nil
	}
	h.Set("Location", "https://taufinity.cloudflareaccess.com/cdn-cgi/access/login/"+StagingHost+"?redirect_url=%2Fapi")
	h.Set("Content-Type", "text/html")
	return &http.Response{StatusCode: 302, Header: h, Body: io.NopCloser(strings.NewReader("<html>login</html>")), Request: req}, nil
}

func (f *cacheFixture) post(t *testing.T, gate *accessGate, body string) (*http.Response, error) {
	t.Helper()
	c := &http.Client{Transport: &Transport{Base: gate, Resolver: f.res}}
	req, _ := http.NewRequest(http.MethodPost, "https://"+StagingHost+"/api/cli/device/code", io.NopCloser(strings.NewReader(body)))
	resp, err := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return resp, err
}

func TestRefusedCachedTokenIsRefetchedAndRetriedOnce(t *testing.T) {
	f := newCacheFixture(t)
	f.seedCache(t, cachedID, cachedSecret) // stale: Access now wants the fresh one
	gate := &accessGate{accept: freshID}

	resp, err := f.post(t, gate, `{"client_id":"taufinity-cli"}`)
	if err != nil {
		t.Fatalf("the retry with a re-read token should have succeeded: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n := gate.hits.Load(); n != 2 {
		t.Fatalf("server hits = %d, want 2 (refused, then one retry)", n)
	}
	if n := f.fetches.Load(); n != 2 {
		t.Fatalf("gcloud fetches = %d, want 2 (one re-read of both halves)", n)
	}
	for i, b := range gate.bodies {
		if b != `{"client_id":"taufinity-cli"}` {
			t.Fatalf("attempt %d sent body %q; the retry must resend the same body", i+1, b)
		}
	}
	// The cache now holds the fresh token.
	next := &Resolver{Getenv: f.res.Getenv, CacheDir: f.res.CacheDir}
	if tok, src := next.For(StagingHost); src != SourceCache || tok.ID != freshID {
		t.Fatalf("cache not refreshed after the retry (source %v)", src)
	}
}

func TestRefusedAgainAfterRefetchSaysRotatedOrRevoked(t *testing.T) {
	f := newCacheFixture(t)
	f.seedCache(t, cachedID, cachedSecret)
	gate := &accessGate{accept: "nobody"}

	_, err := f.post(t, gate, `{}`)
	be := AsBlocked(err)
	if be == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	if n := gate.hits.Load(); n != 2 {
		t.Fatalf("server hits = %d, want exactly 2: retry once, not more", n)
	}
	if !strings.Contains(be.Error(), "the Access service token was refused (rotated or revoked?)") {
		t.Fatalf("message = %q", be.Error())
	}
	assertNoSecret(t, be.Error())
}

func TestFreshGcloudTokenRefusedIsNotRetried(t *testing.T) {
	f := newCacheFixture(t) // empty cache: the token comes from gcloud this run
	gate := &accessGate{accept: "nobody"}
	_, err := f.post(t, gate, `{}`)
	if AsBlocked(err) == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	if n := gate.hits.Load(); n != 1 {
		t.Fatalf("server hits = %d; a token read from gcloud this very run gains nothing from a re-read", n)
	}
}

func TestEnvTokenRefusedIsNotRetriedAndNamesTheVariables(t *testing.T) {
	f := newCacheFixture(t)
	f.env[EnvClientID], f.env[EnvClientSecret] = envID, envSecret
	gate := &accessGate{accept: "nobody"}
	_, err := f.post(t, gate, `{}`)
	be := AsBlocked(err)
	if be == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	if n := gate.hits.Load(); n != 1 {
		t.Fatalf("server hits = %d, want 1", n)
	}
	if !strings.Contains(be.Error(), "CF_ACCESS_CLIENT_ID") {
		t.Fatalf("message should name the env vars: %q", be.Error())
	}
	assertNoSecret(t, be.Error())
}

func TestGcloudMissingIsATypedErrorNamingTheLogin(t *testing.T) {
	prev := LookPath
	LookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { LookPath = prev })

	f := newCacheFixture(t)
	f.res.Fetch = GcloudFetch // the real one, with gcloud "missing"
	gate := &accessGate{accept: freshID}
	_, err := f.post(t, gate, `{}`)
	be := AsBlocked(err)
	if be == nil {
		t.Fatalf("want *BlockedError, got %v", err)
	}
	var fe *FetchError
	if !errors.As(be.FetchErr, &fe) || fe.Kind != FetchGcloudMissing {
		t.Fatalf("FetchErr = %v, want a FetchError of kind gcloud-missing", be.FetchErr)
	}
	msg := be.Error()
	for _, want := range []string{
		StagingHost + " is behind Cloudflare Access and no service token was available",
		"gcloud is not installed",
		"Fix: gcloud auth login, or set CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET.",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q\nlacks %q", msg, want)
		}
	}
	// Both halves failed for the same reason: say it once.
	if strings.Count(msg, "gcloud is not installed") != 1 {
		t.Fatalf("reason repeated: %q", msg)
	}
}

func TestClassifyGcloudFailure(t *testing.T) {
	cases := map[string]FetchErrorKind{
		"ERROR: (gcloud.secrets.versions.access) You do not currently have an active account selected.\nPlease run:\n\n  $ gcloud auth login": FetchNotLoggedIn,
		"ERROR: (gcloud.secrets.versions.access) There was a problem refreshing your current auth tokens: Reauthentication failed.":           FetchNotLoggedIn,
		"ERROR: (gcloud.secrets.versions.access) PERMISSION_DENIED: Permission 'secretmanager.versions.access' denied":                        FetchPermissionDenied,
		"ERROR: something else": FetchOther,
	}
	for stderr, want := range cases {
		if got := classifyGcloudFailure(SecretClientID, GCPProject, stderr); got.Kind != want {
			t.Errorf("%q: kind = %v, want %v", stderr, got.Kind, want)
		}
	}
	nl := classifyGcloudFailure(SecretClientID, GCPProject, "You do not currently have an active account selected.")
	if !strings.Contains(nl.Error(), "gcloud auth login") {
		t.Fatalf("not-logged-in message must name the login command: %q", nl.Error())
	}
}

// The error a user sees never carries the token, from any source.
func TestBlockedErrorNeverContainsTheSecret(t *testing.T) {
	for _, be := range []*BlockedError{
		{Host: StagingHost},
		{Host: StagingHost, TokenSent: true, Source: SourceEnv},
		{Host: StagingHost, TokenSent: true, Source: SourceCache},
		{Host: StagingHost, TokenSent: true, Source: SourceGcloud, Refetched: true},
		{Host: StagingHost, TokenSent: true, Source: SourceCache, RefetchErr: errors.New("x")},
	} {
		assertNoSecret(t, be.Error())
	}
}

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, v := range []string{cachedID, cachedSecret, freshID, freshSecret, envID, envSecret, fakeID, fakeSecret} {
		if strings.Contains(s, v) {
			t.Fatalf("message leaked a token value: %q", s)
		}
	}
}
