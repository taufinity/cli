// Package cfaccess gets the CLI through Cloudflare Access on staging.
//
// staging-studio.taufinity.io sits behind Cloudflare Access, a gate in front of
// the app that turns away any request without a valid service token (a pair of
// CF-Access-Client-Id / CF-Access-Client-Secret headers). Without it Access
// answers with a redirect to its own interactive login page, which the CLI used
// to report as "invalid character '<' looking for beginning of value".
//
// Every Studio-bound HTTP client in the CLI routes through Transport, so there
// is one place that decides which token, if any, rides a request. Resolver.For
// picks it, in this order:
//
//  1. CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET from the environment, when
//     both are set. These go to whichever Studio host the client targets, the
//     same as before this package existed: the user chose them explicitly.
//  2. Otherwise, and ONLY when the request host is in accessHosts (staging):
//     a. the cache file ~/.config/taufinity/cf-access-<host>.json (0600,
//     written atomically, the same discipline as credentials.json);
//     b. on a cache miss, Secret Manager read with the caller's own gcloud
//     login (`gcloud secrets versions access latest`), which then fills the
//     cache, so later commands in any shell, IDE, MCP server or agent need
//     neither gcloud nor an exported variable.
//
// Hosts not in accessHosts (production included) never get a cached or
// gcloud-fetched token and never trigger gcloud.
//
// When Access still blocks a request, Transport returns a *BlockedError that
// says what to do, instead of handing an HTML page to a JSON decoder. When a
// cached token is refused (rotated), the cache is dropped, the token re-read
// from gcloud once, and the request retried once.
//
// The token values are never logged, printed, or put in an error message.
package cfaccess

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/taufinity/cli/internal/config"
)

const (
	// StagingHost is the staging Studio host.
	StagingHost = "staging-studio.taufinity.io"

	// GCPProject holds the staging service-token secrets.
	GCPProject = "content-gen-484211"

	// SecretClientID and SecretClientSecret are the Secret Manager names of the
	// two halves of the staging service token. The ai-site-gen Makefile targets
	// staging-cf-access-env and staging-cli-login read the same two secrets.
	SecretClientID     = "sitegen-cf-access-client-id"
	SecretClientSecret = "sitegen-cf-access-client-secret"

	// EnvClientID and EnvClientSecret override the lookup when both are set.
	EnvClientID     = "CF_ACCESS_CLIENT_ID"
	EnvClientSecret = "CF_ACCESS_CLIENT_SECRET"

	// HeaderClientID and HeaderClientSecret are the headers Access reads.
	HeaderClientID     = "CF-Access-Client-Id"
	HeaderClientSecret = "CF-Access-Client-Secret"

	// DefaultFetchTimeout bounds the gcloud lookup, so a gcloud that wants an
	// interactive re-login cannot hang the command.
	DefaultFetchTimeout = 10 * time.Second
)

// HostSpec says where the service token for one Access-fronted host lives.
type HostSpec struct {
	Project      string // GCP project holding the secrets
	SecretID     string // Secret Manager name of the client id
	SecretSecret string // Secret Manager name of the client secret
}

// accessHosts is the table of hosts known to sit behind Cloudflare Access, and
// where their service token lives. It is data on purpose: a new Access-fronted
// environment is one line here, not a new branch in the code. Any host added
// here receives that token, so keep it to hosts genuinely behind Access.
var accessHosts = map[string]HostSpec{
	StagingHost: {Project: GCPProject, SecretID: SecretClientID, SecretSecret: SecretClientSecret},
}

func lookupHost(host string) (HostSpec, bool) {
	spec, ok := accessHosts[strings.ToLower(host)]
	return spec, ok
}

// IsAutoHost reports whether host (a bare hostname, no port) is a known
// Access-fronted host that gets the cached or gcloud-fetched token.
func IsAutoHost(host string) bool {
	_, ok := lookupHost(host)
	return ok
}

// Token is a Cloudflare Access service token. Its String method never reveals
// the values, so an accidental %v in a log line prints nothing sensitive.
type Token struct {
	ID     string
	Secret string
}

func (t Token) valid() bool { return t.ID != "" && t.Secret != "" }

// String implements fmt.Stringer without exposing the values.
func (t Token) String() string {
	if !t.valid() {
		return "cfaccess.Token(empty)"
	}
	return "cfaccess.Token(set)"
}

// GoString keeps %#v from printing the fields.
func (t Token) GoString() string { return t.String() }

// FetchFunc reads the latest version of one secret in project. Tests replace
// it with a fake.
type FetchFunc func(ctx context.Context, project, secretName string) (string, error)

// Source says where a token came from.
type Source int

const (
	// SourceNone means no token is sent.
	SourceNone Source = iota
	// SourceEnv means CF_ACCESS_CLIENT_ID/SECRET.
	SourceEnv
	// SourceCache means the cache file under ~/.config/taufinity.
	SourceCache
	// SourceGcloud means Secret Manager, via gcloud, in this process.
	SourceGcloud
	// SourceRequest means the caller set the headers on the request itself.
	SourceRequest
)

// String is a short label for status output and error messages.
func (s Source) String() string {
	switch s {
	case SourceEnv:
		return "environment (CF_ACCESS_CLIENT_ID/CF_ACCESS_CLIENT_SECRET)"
	case SourceCache:
		return "cache"
	case SourceGcloud:
		return "gcloud (Secret Manager)"
	case SourceRequest:
		return "request headers"
	default:
		return "none"
	}
}

// Resolver decides which service token, if any, a request to a host gets.
// Safe for concurrent use. Use NewResolver for the real one; a struct literal
// works in tests (nil CacheDir = no cache file).
type Resolver struct {
	// Getenv reads the environment. Read on every call, so a test (or a user)
	// changing the variables is honoured.
	Getenv func(string) string
	// Fetch reads a secret from Secret Manager.
	Fetch FetchFunc
	// Timeout bounds the whole fetch of both halves.
	Timeout time.Duration
	// CacheDir returns the directory for the cache files. nil = no disk cache.
	CacheDir func() string

	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	resolved bool
	tok      Token
	src      Source
	err      error // why no token could be had, when tok is empty
}

// NewResolver returns a Resolver that reads the environment, then the cache
// file, then gcloud.
func NewResolver() *Resolver {
	return &Resolver{
		Getenv:   os.Getenv,
		Fetch:    GcloudFetch,
		Timeout:  DefaultFetchTimeout,
		CacheDir: config.Dir,
	}
}

func (r *Resolver) state(host string) *hostState {
	if r.hosts == nil {
		r.hosts = map[string]*hostState{}
	}
	st, ok := r.hosts[host]
	if !ok {
		st = &hostState{}
		r.hosts[host] = st
	}
	return st
}

// For returns the token to send to host, and where it came from. The
// environment wins; then, for a known Access host only, the cache file, then
// gcloud (at most once per Resolver and host, unless Refetch is called).
func (r *Resolver) For(host string) (Token, Source) {
	if tok := (Token{ID: r.Getenv(EnvClientID), Secret: r.Getenv(EnvClientSecret)}); tok.valid() {
		return tok, SourceEnv
	}
	host = strings.ToLower(host)
	spec, ok := lookupHost(host)
	if !ok {
		return Token{}, SourceNone
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(host)
	if !st.resolved {
		st.resolved = true
		if tok, ok := r.readCache(host); ok {
			st.tok, st.src = tok, SourceCache
		} else {
			r.fetchLocked(host, spec, st)
		}
	}
	return st.tok, st.src
}

// Refetch drops host's cache file and in-memory token, reads the token from
// gcloud again and caches it. Transport calls it once when Access refuses a
// cached token, which is what a rotated service token looks like.
func (r *Resolver) Refetch(host string) (Token, error) {
	host = strings.ToLower(host)
	spec, ok := lookupHost(host)
	if !ok {
		return Token{}, fmt.Errorf("%s is not a known Cloudflare Access host", host)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeCache(host)
	st := r.state(host)
	*st = hostState{resolved: true}
	r.fetchLocked(host, spec, st)
	return st.tok, st.err
}

// FetchError is why no token could be had for host, or nil when one was found
// or the lookup never ran.
func (r *Resolver) FetchError(host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.hosts[strings.ToLower(host)]; ok {
		return st.err
	}
	return nil
}

// fetchLocked reads both halves from gcloud into st and, on success, writes
// the cache. Caller holds r.mu, which also makes concurrent first requests
// share one fetch.
func (r *Resolver) fetchLocked(host string, spec HostSpec, st *hostState) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	// A fresh context, not the request's: a cancelled first request must not
	// poison the result for the rest of the process.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fetch := r.Fetch
	if fetch == nil {
		fetch = GcloudFetch
	}
	var (
		wg               sync.WaitGroup
		id, secret       string
		idErr, secretErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); id, idErr = fetch(ctx, spec.Project, spec.SecretID) }()
	go func() { defer wg.Done(); secret, secretErr = fetch(ctx, spec.Project, spec.SecretSecret) }()
	wg.Wait()

	if err := firstFetchError(idErr, secretErr); err != nil {
		st.err = err
		return
	}
	tok := Token{ID: strings.TrimSpace(id), Secret: strings.TrimSpace(secret)}
	if !tok.valid() {
		st.err = &FetchError{Kind: FetchEmpty, Project: spec.Project}
		return
	}
	st.tok, st.src = tok, SourceGcloud
	// A cache write failure only costs a gcloud call next time; the token in
	// hand is good, so it is not an error for this request.
	_ = r.writeCache(host, tok)
}

// firstFetchError returns one error for the pair. When both halves fail for
// the same reason (gcloud missing, not logged in) one message is enough.
func firstFetchError(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	var fa, fb *FetchError
	if errors.As(a, &fa) && errors.As(b, &fb) && fa.Kind == fb.Kind && fa.Kind != FetchOther {
		return a
	}
	return errors.Join(a, b)
}

// FetchErrorKind classifies why gcloud produced no secret.
type FetchErrorKind int

const (
	// FetchOther is any failure not classified below.
	FetchOther FetchErrorKind = iota
	// FetchGcloudMissing means gcloud is not installed or not on PATH.
	FetchGcloudMissing
	// FetchNotLoggedIn means gcloud has no usable login.
	FetchNotLoggedIn
	// FetchPermissionDenied means the gcloud account may not read the secret.
	FetchPermissionDenied
	// FetchTimeout means gcloud did not answer in time.
	FetchTimeout
	// FetchEmpty means gcloud answered with an empty secret.
	FetchEmpty
)

// FetchError says why the gcloud lookup produced no token, and what to run.
// Detail is gcloud's own last stderr line, which never carries the secret.
type FetchError struct {
	Kind    FetchErrorKind
	Secret  string
	Project string
	Detail  string
}

func (e *FetchError) Error() string {
	switch e.Kind {
	case FetchGcloudMissing:
		return "gcloud is not installed or not on PATH (install the Google Cloud SDK, then run: gcloud auth login)"
	case FetchNotLoggedIn:
		return "gcloud is not logged in (run: gcloud auth login)"
	case FetchPermissionDenied:
		return fmt.Sprintf("your gcloud account may not read secret %s in project %s: %s", e.Secret, e.Project, e.Detail)
	case FetchTimeout:
		return fmt.Sprintf("gcloud did not answer within the time limit reading %s (if it wants a re-login, run: gcloud auth login)", e.Secret)
	case FetchEmpty:
		return fmt.Sprintf("gcloud returned an empty secret in project %s", e.Project)
	default:
		return fmt.Sprintf("gcloud could not read %s: %s", e.Secret, e.Detail)
	}
}

// LookPath finds gcloud. A variable so tests can pretend it is missing.
var LookPath = exec.LookPath

// GcloudFetch reads the latest version of secretName from project with the
// caller's gcloud credentials. The value travels only through stdout into
// memory. stderr (which never carries the value) supplies the error reason.
func GcloudFetch(ctx context.Context, project, secretName string) (string, error) {
	bin, err := LookPath("gcloud")
	if err != nil {
		return "", &FetchError{Kind: FetchGcloudMissing, Secret: secretName, Project: project}
	}
	cmd := exec.CommandContext(ctx, bin, "secrets", "versions", "access", "latest",
		"--secret="+secretName, "--project="+project, "--quiet")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", &FetchError{Kind: FetchGcloudMissing, Secret: secretName, Project: project}
		}
		if ctx.Err() != nil {
			return "", &FetchError{Kind: FetchTimeout, Secret: secretName, Project: project}
		}
		return "", classifyGcloudFailure(secretName, project, stderr.String())
	}
	return stdout.String(), nil
}

// classifyGcloudFailure turns gcloud's stderr into a FetchError kind.
func classifyGcloudFailure(secretName, project, stderr string) *FetchError {
	detail := lastLine(stderr)
	low := strings.ToLower(stderr)
	kind := FetchOther
	switch {
	case strings.Contains(low, "gcloud auth login"),
		strings.Contains(low, "do not currently have an active account"),
		strings.Contains(low, "no active account"),
		strings.Contains(low, "reauthentication"),
		strings.Contains(low, "refresh your current auth tokens"),
		strings.Contains(low, "invalid_grant"):
		kind = FetchNotLoggedIn
	case strings.Contains(low, "permission_denied"),
		strings.Contains(low, "permission denied"),
		strings.Contains(low, "does not have"):
		kind = FetchPermissionDenied
	}
	return &FetchError{Kind: kind, Secret: secretName, Project: project, Detail: detail}
}

// lastLine returns the last non-empty line of s, capped, for a short reason.
func lastLine(s string) string {
	var last string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if last == "" {
		return "no detail from gcloud"
	}
	if len(last) > 200 {
		last = last[:200] + "..."
	}
	return last
}

var (
	defaultMu       sync.RWMutex
	defaultResolver = NewResolver()
	defaultBase     http.RoundTripper // nil = http.DefaultTransport
)

// Default returns the process-wide resolver, so the lookup happens once per
// process no matter how many clients are built.
func Default() *Resolver {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultResolver
}

func baseTransport() http.RoundTripper {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	if defaultBase != nil {
		return defaultBase
	}
	return http.DefaultTransport
}

// SetForTest swaps the process-wide resolver and the underlying transport, so a
// test in another package can route a real staging URL to an httptest server
// and fake gcloud. Returns a restore func. Not for production code.
func SetForTest(r *Resolver, base http.RoundTripper) (restore func()) {
	defaultMu.Lock()
	prevR, prevB := defaultResolver, defaultBase
	defaultResolver, defaultBase = r, base
	defaultMu.Unlock()
	return func() {
		defaultMu.Lock()
		defaultResolver, defaultBase = prevR, prevB
		defaultMu.Unlock()
	}
}

// Transport injects the service token and turns an Access block into a
// *BlockedError. Use it for Studio-bound clients only: with environment
// variables set, the token goes to whatever host the client targets.
type Transport struct {
	// Base performs the request. nil = the process default (http.DefaultTransport).
	Base http.RoundTripper
	// Resolver picks the token. nil = Default().
	Resolver *Resolver
}

// NewTransport wraps base (nil = default) with the process-wide resolver.
func NewTransport(base http.RoundTripper) *Transport {
	return &Transport{Base: base}
}

// NewClient returns an *http.Client with the given timeout that routes through
// Transport.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport(nil)}
}

// withToken returns a clone of req carrying tok. RoundTrippers must not modify
// the caller's request; setting the headers on a clone also keeps them off any
// redirect hop the http.Client follows, since it copies the original headers.
func withToken(req *http.Request, tok Token) *http.Request {
	out := req.Clone(req.Context())
	out.Header.Set(HeaderClientID, tok.ID)
	out.Header.Set(HeaderClientSecret, tok.Secret)
	return out
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	res := t.Resolver
	if res == nil {
		res = Default()
	}
	base := t.Base
	if base == nil {
		base = baseTransport()
	}

	host := strings.ToLower(req.URL.Hostname())
	out := req
	src := SourceNone
	var tok Token
	if req.Header.Get(HeaderClientID) != "" && req.Header.Get(HeaderClientSecret) != "" {
		src = SourceRequest
	} else {
		tok, src = res.For(host)
		// The automatic token is only sent over TLS: an http:// URL must not
		// carry it in clear text.
		if (src == SourceCache || src == SourceGcloud) && req.URL.Scheme != "https" {
			tok, src = Token{}, SourceNone
		}
		if src != SourceNone {
			out = withToken(req, tok)
		}
	}

	// A cached token may be stale (rotated); then the request is retried once
	// with a freshly read one, which needs the body a second time.
	canRetry := src == SourceCache
	if canRetry && req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		data, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		getBody := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		out.Body, _ = getBody()
		out.GetBody = getBody
	}

	resp, err := base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if !blocked(req, resp) {
		return resp, nil
	}
	drain(resp)

	be := &BlockedError{Host: host, TokenSent: src != SourceNone, Source: src}
	switch src {
	case SourceNone:
		be.FetchErr = res.FetchError(host)
		return nil, be
	case SourceCache:
		// The cached token was refused: re-read it from gcloud and try once more.
		fresh, ferr := res.Refetch(host)
		if ferr != nil {
			be.RefetchErr = ferr
			return nil, be
		}
		retry := withToken(req, fresh)
		if out.GetBody != nil {
			body, err := out.GetBody()
			if err != nil {
				return nil, err
			}
			retry.Body = body
		}
		resp, err = base.RoundTrip(retry)
		if err != nil {
			return nil, err
		}
		if !blocked(req, resp) {
			return resp, nil
		}
		drain(resp)
		be.Source, be.Refetched = SourceGcloud, true
		return nil, be
	default:
		return nil, be
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// blocked reports whether resp is Cloudflare Access turning the request away.
// Two signs: a redirect to the Access login on *.cloudflareaccess.com (any
// host), or an HTML page from a known Access host where the CLI expected JSON.
// Transport returns before the http.Client could follow such a redirect, so
// the login page is never fetched.
func blocked(req *http.Request, resp *http.Response) bool {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && isAccessLoginHost(u.Hostname()) {
				return true
			}
		}
	}
	if IsAutoHost(req.URL.Hostname()) && isStudioAPIPath(req.URL.Path) {
		mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mt == "text/html" {
			return true
		}
	}
	return false
}

func isAccessLoginHost(h string) bool {
	h = strings.ToLower(h)
	return h == "cloudflareaccess.com" || strings.HasSuffix(h, ".cloudflareaccess.com")
}

// isStudioAPIPath is the set of paths that answer JSON (or an MCP stream),
// never an HTML page, so HTML there can only be Access.
func isStudioAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

// ErrBlocked matches any *BlockedError via errors.Is.
var ErrBlocked = errors.New("blocked by Cloudflare Access")

// fixHint is the advice for a request that went out without a token.
const fixHint = "Fix: gcloud auth login, or set CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET."

// BlockedError says Cloudflare Access turned a request away, and how to fix it.
// It never contains a token value.
type BlockedError struct {
	Host       string
	TokenSent  bool   // a token went out and Access still refused it
	Source     Source // where the refused token came from
	Refetched  bool   // the token was re-read from gcloud and still refused
	FetchErr   error  // why no token was available, when none was sent
	RefetchErr error  // why re-reading a refused cached token failed
}

func (e *BlockedError) Error() string {
	host := e.Host
	if host == "" {
		host = "this server"
	}
	if !e.TokenSent {
		why := "CF_ACCESS_CLIENT_ID/CF_ACCESS_CLIENT_SECRET are not set"
		switch {
		case e.FetchErr != nil:
			why = e.FetchErr.Error()
		case !IsAutoHost(e.Host):
			why += " and the CLI knows no service token for this host"
		}
		return fmt.Sprintf("%s is behind Cloudflare Access and no service token was available: %s. %s", host, strings.TrimRight(why, ". "), fixHint)
	}
	switch {
	case e.Source == SourceEnv:
		return fmt.Sprintf("%s is behind Cloudflare Access and refused the service token from CF_ACCESS_CLIENT_ID/CF_ACCESS_CLIENT_SECRET (rotated or revoked?): "+
			"check those variables, or unset them so the CLI reads the current token with gcloud", host)
	case e.RefetchErr != nil:
		return fmt.Sprintf("%s is behind Cloudflare Access and refused the cached service token, and reading a fresh one failed: %s. %s",
			host, e.RefetchErr.Error(), fixHint)
	case e.Refetched || e.Source == SourceGcloud:
		return fmt.Sprintf("%s is behind Cloudflare Access and the Access service token was refused (rotated or revoked?), "+
			"even freshly read from Secret Manager: check the token in the Access policy for %s", host, host)
	default:
		return fmt.Sprintf("%s is behind Cloudflare Access and refused the service token from %s (rotated or revoked?)", host, e.Source)
	}
}

// Is makes errors.Is(err, ErrBlocked) true.
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

// NonRetryable tells a retrying client to give up at once: retrying an
// Access block only repeats it (Transport already did its one refetch).
func (e *BlockedError) NonRetryable() bool { return true }

// AsBlocked returns the *BlockedError in err's chain, or nil. Callers use it to
// print the actionable message without the URL/transport wrapping around it.
func AsBlocked(err error) *BlockedError {
	var be *BlockedError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// Status describes, for display, whether a token is in use for host and where
// it came from. It may run the gcloud lookup (and fill the cache). It never
// returns the token itself.
func (r *Resolver) Status(host string) (inUse bool, src Source, err error) {
	if !IsAutoHost(host) {
		_, src = r.For(host) // only the env override applies
		return src != SourceNone, src, nil
	}
	tok, src := r.For(host)
	return tok.valid(), src, r.FetchError(host)
}

// CachePath is the cache file for host under the resolver's cache dir, or ""
// when the resolver has no cache.
func (r *Resolver) CachePath(host string) string {
	if r.CacheDir == nil {
		return ""
	}
	return cacheFile(r.CacheDir(), host)
}
