// Package cfaccess gets the CLI through Cloudflare Access on staging.
//
// staging-studio.taufinity.io sits behind Cloudflare Access, a gate in front of
// the app that turns away any request without a valid service token (a pair of
// CF-Access-Client-Id / CF-Access-Client-Secret headers). Without it Access
// answers with a redirect to its own interactive login page, which the CLI used
// to report as "invalid character '<' looking for beginning of value".
//
// Every Studio-bound HTTP client in the CLI routes through Transport, so there
// is one place that decides which token, if any, rides a request:
//
//  1. CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET from the environment, when
//     both are set. These go to whichever Studio host the client targets, the
//     same as before this package existed: the user chose them explicitly.
//  2. Otherwise, and ONLY when the request host is in autoHosts (staging), the
//     token is read from Secret Manager with the caller's own gcloud login,
//     once per process, and kept in memory. It is never written to disk, never
//     logged, never sent to telemetry, and never sent to any other host,
//     production included.
//
// When Access still blocks a request, Transport returns a *BlockedError that
// says what to do, instead of handing an HTML page to a JSON decoder.
package cfaccess

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// StagingHost is the staging Studio host. It is the only host the
	// gcloud-fetched token is ever sent to; see autoHosts.
	StagingHost = "staging-studio.taufinity.io"

	// GCPProject holds the service-token secrets.
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

// autoHosts is the allowlist of hosts that get the gcloud-fetched token. Keep
// it to hosts that are genuinely behind Access with this service token: any
// host added here receives the staging credential.
var autoHosts = map[string]bool{
	StagingHost: true,
}

// IsAutoHost reports whether host (a bare hostname, no port) gets the
// gcloud-fetched token.
func IsAutoHost(host string) bool {
	return autoHosts[strings.ToLower(host)]
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

// FetchFunc reads one secret's latest version. Tests replace it with a fake.
type FetchFunc func(ctx context.Context, secretName string) (string, error)

// Resolver decides which service token, if any, a request to a host gets.
// Safe for concurrent use. The zero value is not usable; use NewResolver.
type Resolver struct {
	// Getenv reads the environment. Read on every call, so a test (or a user)
	// changing the variables is honoured.
	Getenv func(string) string
	// Fetch reads a secret from Secret Manager.
	Fetch FetchFunc
	// Timeout bounds the whole fetch of both halves.
	Timeout time.Duration

	once     sync.Once
	fetched  Token
	fetchErr error
}

// NewResolver returns a Resolver that reads the environment and falls back to
// gcloud for staging.
func NewResolver() *Resolver {
	return &Resolver{
		Getenv:  os.Getenv,
		Fetch:   GcloudFetch,
		Timeout: DefaultFetchTimeout,
	}
}

// Source says where a token came from.
type Source int

const (
	// SourceNone means no token is sent.
	SourceNone Source = iota
	// SourceEnv means CF_ACCESS_CLIENT_ID/SECRET.
	SourceEnv
	// SourceGcloud means Secret Manager, via gcloud.
	SourceGcloud
)

// For returns the token to send to host, and where it came from. The
// environment wins; the gcloud lookup runs at most once per Resolver and only
// for an allowlisted host.
func (r *Resolver) For(host string) (Token, Source) {
	if tok := (Token{ID: r.Getenv(EnvClientID), Secret: r.Getenv(EnvClientSecret)}); tok.valid() {
		return tok, SourceEnv
	}
	if !IsAutoHost(host) {
		return Token{}, SourceNone
	}
	r.once.Do(r.fetch)
	if r.fetched.valid() {
		return r.fetched, SourceGcloud
	}
	return Token{}, SourceNone
}

// FetchError is why the gcloud lookup produced no token, or nil when it
// succeeded or never ran.
func (r *Resolver) FetchError() error {
	// once guarantees fetchErr is written before Do returns; callers only ask
	// after For, which ran Do. Calling it first is harmless: it reports nil.
	return r.fetchErr
}

func (r *Resolver) fetch() {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	// A fresh context, not the request's: a cancelled first request must not
	// poison the cache for the rest of the process.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var (
		wg               sync.WaitGroup
		id, secret       string
		idErr, secretErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); id, idErr = r.Fetch(ctx, SecretClientID) }()
	go func() { defer wg.Done(); secret, secretErr = r.Fetch(ctx, SecretClientSecret) }()
	wg.Wait()

	if err := errors.Join(idErr, secretErr); err != nil {
		r.fetchErr = err
		return
	}
	tok := Token{ID: strings.TrimSpace(id), Secret: strings.TrimSpace(secret)}
	if !tok.valid() {
		r.fetchErr = errors.New("gcloud returned an empty secret")
		return
	}
	r.fetched = tok
}

// GcloudFetch reads the latest version of secretName from GCPProject with the
// caller's gcloud credentials. The value travels only through stdout into
// memory. stderr (which never carries the value) supplies the error reason.
func GcloudFetch(ctx context.Context, secretName string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "secrets", "versions", "access", "latest",
		"--secret="+secretName, "--project="+GCPProject, "--quiet")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("gcloud is not installed or not on PATH")
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("gcloud did not answer within the time limit reading %s", secretName)
		}
		return "", fmt.Errorf("gcloud could not read %s: %s", secretName, lastLine(stderr.String()))
	}
	return stdout.String(), nil
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

// Default returns the process-wide resolver, so the gcloud lookup happens once
// per process no matter how many clients are built.
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

	host := req.URL.Hostname()
	out := req
	sent := req.Header.Get(HeaderClientID) != "" && req.Header.Get(HeaderClientSecret) != ""
	if !sent {
		tok, src := res.For(host)
		// The gcloud token is staging-only and only over TLS. For() already
		// enforces the host; the scheme check stops an http:// URL from
		// carrying it in clear text.
		if src == SourceGcloud && req.URL.Scheme != "https" {
			tok, src = Token{}, SourceNone
		}
		if src != SourceNone {
			// RoundTrippers must not modify the caller's request. Setting the
			// headers on a clone also keeps them off any redirect hop the
			// http.Client follows, since it copies headers from the original.
			out = req.Clone(req.Context())
			out.Header.Set(HeaderClientID, tok.ID)
			out.Header.Set(HeaderClientSecret, tok.Secret)
			sent = true
		}
	}

	resp, err := base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if blocked(req, resp) {
		resp.Body.Close()
		be := &BlockedError{Host: host, TokenSent: sent}
		if !sent {
			be.FetchErr = res.FetchError()
		}
		return nil, be
	}
	return resp, nil
}

// blocked reports whether resp is Cloudflare Access turning the request away.
// Two signs: a redirect to the Access login on *.cloudflareaccess.com (any
// host), or an HTML page from an allowlisted host where the CLI expected JSON.
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

// BlockedError says Cloudflare Access turned a request away, and how to fix it.
type BlockedError struct {
	Host      string
	TokenSent bool  // a token went out and Access still refused it
	FetchErr  error // why the gcloud lookup gave nothing, if it ran
}

func (e *BlockedError) Error() string {
	if e.TokenSent {
		return "staging is behind Cloudflare Access and rejected the service token that was sent: " +
			"check CF_ACCESS_CLIENT_ID/CF_ACCESS_CLIENT_SECRET, or unset them so the CLI reads the current token with gcloud"
	}
	msg := "staging is behind Cloudflare Access and no service token was available: " +
		"set CF_ACCESS_CLIENT_ID/CF_ACCESS_CLIENT_SECRET " +
		"(eval \"$(make -C ~/Documents/code/ai-site-gen staging-cf-access-env)\") or log in to gcloud"
	if e.FetchErr != nil {
		msg += " (gcloud lookup: " + e.FetchErr.Error() + ")"
	}
	return msg
}

// Is makes errors.Is(err, ErrBlocked) true.
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

// NonRetryable tells a retrying client to give up at once: retrying an
// Access block only repeats it, and a gcloud lookup is never retried anyway.
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
