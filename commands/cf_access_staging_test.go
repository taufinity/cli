package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/cfaccess"
)

// Fake values only; no test touches the real staging token.
const (
	stagingFakeID     = "fake-client-id"
	stagingFakeSecret = "fake-client-secret"
)

type routeAllTo struct{ target *url.URL }

func (r routeAllTo) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = r.target.Scheme, r.target.Host, r.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

// fakeStaging routes every request to serverURL and swaps gcloud for a fake,
// with the CF env vars cleared, so a request to the real staging URL is
// exercised end to end without leaving the machine.
func fakeStaging(t *testing.T, serverURL string) *atomic.Int32 {
	t.Helper()
	t.Setenv(cfaccess.EnvClientID, "")
	t.Setenv(cfaccess.EnvClientSecret, "")
	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	res := cfaccess.NewResolver()
	res.Fetch = func(_ context.Context, name string) (string, error) {
		fetches.Add(1)
		if name == cfaccess.SecretClientID {
			return stagingFakeID, nil
		}
		return stagingFakeSecret, nil
	}
	t.Cleanup(cfaccess.SetForTest(res, routeAllTo{target}))
	return &fetches
}

func withAPIURL(t *testing.T, u string) {
	t.Helper()
	prev := flagAPIURL
	flagAPIURL = u
	t.Cleanup(func() { flagAPIURL = prev })
}

// `taufinity --env staging auth login` with no env vars: the device-code
// request goes through the resolver and carries the gcloud-fetched token.
func TestAuthLogin_DeviceCodeRequestCarriesStagingToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotID, gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cli/device/code" {
			gotID = r.Header.Get(cfaccess.HeaderClientID)
			gotSecret = r.Header.Get(cfaccess.HeaderClientSecret)
		}
		// Stop the flow right after the device-code request.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"stop here"}`))
	}))
	defer srv.Close()
	fetches := fakeStaging(t, srv.URL)
	withAPIURL(t, StagingAPIURL)

	err := runAuthLogin(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("expected the test server's stop error, got %v", err)
	}
	if gotID != stagingFakeID || gotSecret != stagingFakeSecret {
		t.Fatalf("device-code request did not carry the staging token (id set=%v secret set=%v)", gotID != "", gotSecret != "")
	}
	if fetches.Load() == 0 {
		t.Fatal("the gcloud fetch never ran for staging")
	}
}

// The original failure: Access redirects to its login page and login printed
// "decode response: invalid character '<'". Now it is the actionable error.
func TestAuthLogin_AccessRedirectGivesClearError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://taufinity.cloudflareaccess.com/cdn-cgi/access/login/x", http.StatusFound)
	}))
	defer srv.Close()
	fakeStaging(t, srv.URL)
	withAPIURL(t, StagingAPIURL)

	err := runAuthLogin(nil, nil)
	if cfaccess.AsBlocked(err) == nil {
		t.Fatalf("want the Cloudflare Access error, got %v", err)
	}
	if strings.Contains(err.Error(), "decode response") {
		t.Fatalf("still the JSON decode error: %v", err)
	}
}

// The MCP stdio bridge on staging gets the token from the same resolver.
func TestStdioBridge_StagingGetsGcloudToken(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()
	mu.responseBody.Store(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	fakeStaging(t, mu.server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in, out, _, done := runBridgeAsync(t, ctx, StagingAPIURL+"/mcp")
	if _, err := in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	readNextFrame(t, out.(*threadSafeBuffer), 5*time.Second)
	_ = in.Close()
	<-done

	mu.mu.Lock()
	defer mu.mu.Unlock()
	if len(mu.cfIDHeaders) == 0 || mu.cfIDHeaders[0] != stagingFakeID || mu.cfSecretHeaders[0] != stagingFakeSecret {
		t.Fatal("bridge request to staging did not carry the gcloud-fetched token")
	}
}

// And the bridge on production never does.
func TestStdioBridge_ProdGetsNoToken(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()
	mu.responseBody.Store(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	fetches := fakeStaging(t, mu.server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in, out, _, done := runBridgeAsync(t, ctx, DefaultAPIURL+"/mcp")
	if _, err := in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	readNextFrame(t, out.(*threadSafeBuffer), 5*time.Second)
	_ = in.Close()
	<-done

	mu.mu.Lock()
	defer mu.mu.Unlock()
	for _, id := range mu.cfIDHeaders {
		if id != "" {
			t.Fatal("production MCP request carried a CF-Access header")
		}
	}
	if fetches.Load() != 0 {
		t.Fatal("gcloud ran for a production bridge")
	}
}
