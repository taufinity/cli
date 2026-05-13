package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mockUpstream simulates the Studio /mcp endpoint over Streamable HTTP.
// It captures inbound requests so tests can assert on headers and bodies.
type mockUpstream struct {
	server *httptest.Server

	mu             sync.Mutex
	authHeaders    []string
	userAgents     []string
	methods        []string
	requestBodies  [][]byte
	responseStatus atomic.Int32
	responseBody   atomic.Value // string
}

func newMockUpstream() *mockUpstream {
	mu := &mockUpstream{}
	mu.responseStatus.Store(int32(http.StatusOK))
	mu.responseBody.Store("")
	mu.server = httptest.NewServer(http.HandlerFunc(mu.handle))
	return mu
}

func (m *mockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.authHeaders = append(m.authHeaders, r.Header.Get("Authorization"))
	m.userAgents = append(m.userAgents, r.Header.Get("User-Agent"))
	m.requestBodies = append(m.requestBodies, body)
	var frame struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &frame)
	m.methods = append(m.methods, frame.Method)
	m.mu.Unlock()

	status := int(m.responseStatus.Load())
	if status != http.StatusOK && status != http.StatusAccepted {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"upstream"}`))
		return
	}

	respBody := m.responseBody.Load().(string)
	if respBody == "" {
		// Default: echo the id back as a JSON-RPC success result.
		var inbound struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &inbound)
		respBody = `{"jsonrpc":"2.0","id":` + string(inbound.ID) + `,"result":{"ok":true}}`
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(respBody))
}

func (m *mockUpstream) close() { m.server.Close() }

func (m *mockUpstream) url() string { return m.server.URL + "/mcp" }

// runBridgeAsync starts the bridge in a goroutine. Caller writes frames to inputW.
// done is closed when the bridge returns.
func runBridgeAsync(t *testing.T, ctx context.Context, upstream string) (
	inputW io.WriteCloser,
	outputR io.Reader,
	stderr *bytes.Buffer,
	done chan struct{},
) {
	t.Helper()

	pr, pw := io.Pipe()
	outBuf := &threadSafeBuffer{}
	errBuf := &bytes.Buffer{}

	d := make(chan struct{})

	go func() {
		defer close(d)
		_ = RunStdioBridge(ctx, StdioBridgeConfig{
			UpstreamURL: upstream,
			Token:       "test-token",
			UserAgent:   "taufinity-cli/test (mcp-stdio)",
			Timeout:     5 * time.Second,
			Stdin:       pr,
			Stdout:      outBuf,
			Stderr:      errBuf,
		})
	}()

	return pw, outBuf, errBuf, d
}

// threadSafeBuffer wraps bytes.Buffer with a mutex.
type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *threadSafeBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

func (t *threadSafeBuffer) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Read(p)
}

func (t *threadSafeBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// readNextFrame reads one JSON line from the buffer, retrying briefly to
// handle the bridge's async write timing.
//
// The drain step takes the lock for the entire read-modify-write sequence
// to avoid losing concurrent writes that arrive between snapshot and reset.
// (Earlier revisions called String() to snapshot, then Lock+Reset+WriteString
// the remainder — anything written between snapshot and Reset was discarded,
// which dropped server-pushed notifications that arrived back-to-back with
// tool results in TestStdioBridge_AgainstRealStreamableHTTPServer.)
func readNextFrame(t *testing.T, buf *threadSafeBuffer, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		buf.mu.Lock()
		s := buf.buf.String()
		idx := strings.IndexByte(s, '\n')
		if idx >= 0 {
			line := []byte(s[:idx])
			rest := s[idx+1:]
			buf.buf.Reset()
			buf.buf.WriteString(rest)
			buf.mu.Unlock()
			return line
		}
		buf.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	buf.mu.Lock()
	have := buf.buf.String()
	buf.mu.Unlock()
	t.Fatalf("no frame on stdout within %s; have: %q", timeout, have)
	return nil
}

func TestStdioBridge_ForwardsToolsList(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mu.responseBody.Store(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"foo"}]}}`)

	in, out, _, done := runBridgeAsync(t, ctx, mu.url())

	// Send a tools/list request.
	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	if _, err := in.Write([]byte(frame)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	line := readNextFrame(t, out.(*threadSafeBuffer), 5*time.Second)

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode response: %v: %s", err, line)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if !strings.Contains(string(resp.Result), `"foo"`) {
		t.Errorf("result missing tool: %s", resp.Result)
	}

	// id round-trips as float64 = 1
	if v, ok := resp.ID.(float64); !ok || v != 1 {
		t.Errorf("id round-trip = %v (%T), want 1", resp.ID, resp.ID)
	}

	// Verify upstream received the right thing.
	mu.mu.Lock()
	gotMethods := append([]string(nil), mu.methods...)
	gotAuth := append([]string(nil), mu.authHeaders...)
	gotUA := append([]string(nil), mu.userAgents...)
	mu.mu.Unlock()

	if len(gotMethods) == 0 || gotMethods[0] != "tools/list" {
		t.Errorf("upstream methods = %v, want first = tools/list", gotMethods)
	}
	if len(gotAuth) == 0 || gotAuth[0] != "Bearer test-token" {
		t.Errorf("upstream Authorization = %v, want Bearer test-token", gotAuth)
	}
	if len(gotUA) == 0 || !strings.HasPrefix(gotUA[0], "taufinity-cli/") {
		t.Errorf("upstream User-Agent = %v, want prefix taufinity-cli/", gotUA)
	}

	_ = in.Close()
	<-done
}

func TestStdioBridge_AuthFailedMapsTo32001(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()
	mu.responseStatus.Store(int32(http.StatusUnauthorized))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsync(t, ctx, mu.url())

	frame := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n"
	_, _ = in.Write([]byte(frame))

	line := readNextFrame(t, out.(*threadSafeBuffer), 5*time.Second)

	var resp struct {
		Error struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode error frame: %v: %s", err, line)
	}
	if resp.Error.Code != errCodeAuthFailed {
		t.Errorf("code = %d, want %d", resp.Error.Code, errCodeAuthFailed)
	}
	if !strings.Contains(resp.Error.Message, "taufinity auth login") {
		t.Errorf("message = %q, want it to contain 'taufinity auth login' so MCP clients can surface a usable fix", resp.Error.Message)
	}
	if resp.Error.Data["error"] != "auth_failed" {
		t.Errorf("data.error = %v, want auth_failed", resp.Error.Data["error"])
	}
	if hint, _ := resp.Error.Data["hint"].(string); !strings.Contains(hint, "taufinity auth login") {
		t.Errorf("data.hint = %v, want hint to contain 'taufinity auth login'", resp.Error.Data["hint"])
	}

	_ = in.Close()
	<-done
}

// TestDegradedBridge_RepliesAuthFailedToInitialize verifies that when the
// startup credential probe fails, the bridge still answers the MCP client's
// `initialize` request with a JSON-RPC error frame containing a human-readable
// remediation message — instead of exiting before the handshake completes,
// which would surface as a useless "server disconnected" popup in the client.
func TestDegradedBridge_RepliesAuthFailedToInitialize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	expiry := time.Date(2026, 5, 12, 17, 45, 23, 0, time.UTC)
	authErr := &startupAuthError{
		summary:   "Taufinity credentials expired",
		detail:    "token expired at 2026-05-12T17:45:23Z",
		expiresAt: expiry,
	}

	stdinR, stdinW := io.Pipe()
	stdout := &threadSafeBuffer{}
	stderr := &bytes.Buffer{}
	done := make(chan error, 1)

	go func() {
		done <- runDegradedBridge(ctx, stdinR, stdout, stderr, authErr)
	}()

	// Client sends initialize (a real MCP handshake — the first thing every client sends).
	initFrame := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"0.0.1"}}}` + "\n"
	if _, err := stdinW.Write([]byte(initFrame)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	line := readNextFrame(t, stdout, 2*time.Second)

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode error frame: %v: %s", err, line)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if string(resp.ID) != "0" {
		t.Errorf("id = %s, want 0 (must echo the request id so the client can correlate)", resp.ID)
	}
	if resp.Error.Code != errCodeAuthFailed {
		t.Errorf("code = %d, want %d", resp.Error.Code, errCodeAuthFailed)
	}
	if !strings.Contains(resp.Error.Message, "taufinity auth login") {
		t.Errorf("message = %q, want it to contain 'taufinity auth login'", resp.Error.Message)
	}
	if resp.Error.Data["error"] != "auth_failed" {
		t.Errorf("data.error = %v, want auth_failed", resp.Error.Data["error"])
	}
	if got := resp.Error.Data["expired_at"]; got != "2026-05-12T17:45:23Z" {
		t.Errorf("data.expired_at = %v, want 2026-05-12T17:45:23Z", got)
	}
	if hint, _ := resp.Error.Data["hint"].(string); !strings.Contains(hint, "taufinity auth login") {
		t.Errorf("data.hint = %v, want it to contain 'taufinity auth login'", resp.Error.Data["hint"])
	}
	if _, ok := resp.Error.Data["trace_id"].(string); !ok {
		t.Errorf("data.trace_id missing or not a string: %v", resp.Error.Data["trace_id"])
	}

	_ = stdinW.Close()
	select {
	case err := <-done:
		if err != nil && err != io.EOF && err != context.Canceled {
			t.Errorf("runDegradedBridge returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDegradedBridge did not return after stdin closed")
	}
}

// TestDegradedBridge_DropsNotifications verifies that JSON-RPC notifications
// (requests with no id, or with id explicitly null) get no reply — replying
// to a notification is a protocol violation that some MCP clients will treat
// as a fatal error.
//
// We use a sentinel-request strategy instead of a timing sleep: send
// [notification, request], assert the only reply we see has the request's
// id. Ordering preserves causality (frames are read line-by-line), so any
// reply to the notification would appear before the sentinel reply.
func TestDegradedBridge_DropsNotifications(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authErr := &startupAuthError{
		summary: "no Taufinity credentials found",
		detail:  "no credentials file on disk; run 'taufinity auth login' to sign in",
	}

	stdinR, stdinW := io.Pipe()
	stdout := &threadSafeBuffer{}
	stderr := &bytes.Buffer{}
	done := make(chan error, 1)

	go func() {
		done <- runDegradedBridge(ctx, stdinR, stdout, stderr, authErr)
	}()

	// Three frames: missing-id notification, explicit-null-id notification,
	// then a real request as sentinel. If either notification produced a
	// reply, it would arrive before the sentinel reply.
	frames := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}` + "\n" +
		`{"jsonrpc":"2.0","id":"sentinel","method":"tools/list"}` + "\n"
	if _, err := stdinW.Write([]byte(frames)); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	line := readNextFrame(t, stdout, 2*time.Second)
	var resp struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, line)
	}
	if string(resp.ID) != `"sentinel"` {
		t.Errorf("first response id = %s, want \"sentinel\" — earlier notification leaked a reply", resp.ID)
	}

	_ = stdinW.Close()
	<-done

	// After bridge exits, stdout is stable. Verify exactly one frame total.
	if got := strings.Count(strings.TrimRight(stdout.String(), "\n"), "\n"); got != 0 {
		t.Errorf("expected exactly one response frame total after EOF, got %d extra newlines: %s", got, stdout.String())
	}
}

// TestDegradedBridge_MalformedJSONLogged verifies that a malformed JSON line
// on stdin is skipped without crashing the bridge, and that a diagnostic is
// written to stderr so operators can debug a misbehaving client.
func TestDegradedBridge_MalformedJSONLogged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authErr := &startupAuthError{summary: "no Taufinity credentials found", detail: "no credentials file on disk"}

	stdinR, stdinW := io.Pipe()
	stdout := &threadSafeBuffer{}
	stderr := &threadSafeBuffer{}
	done := make(chan error, 1)

	go func() {
		done <- runDegradedBridge(ctx, stdinR, stdout, stderr, authErr)
	}()

	// Garbage line, then a valid sentinel request. The bridge must skip the
	// garbage, log it, and still reply to the sentinel.
	frames := `not json at all` + "\n" +
		`{"jsonrpc":"2.0","id":42,"method":"tools/list"}` + "\n"
	if _, err := stdinW.Write([]byte(frames)); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	line := readNextFrame(t, stdout, 2*time.Second)
	var resp struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, line)
	}
	if string(resp.ID) != "42" {
		t.Errorf("response id = %s, want 42", resp.ID)
	}

	_ = stdinW.Close()
	<-done

	if !strings.Contains(stderr.String(), "invalid JSON") {
		t.Errorf("stderr did not log malformed JSON; got: %s", stderr.String())
	}
}

// TestProbeStartupAuth_NoCredentials verifies the probe surfaces a helpful
// summary/detail pair when no credentials file exists on disk.
func TestProbeStartupAuth_NoCredentials(t *testing.T) {
	// Point HOME at an empty temp dir so the credentials loader sees no file.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)

	err := probeStartupAuth()
	if err == nil {
		t.Fatal("probeStartupAuth() = nil, want startupAuthError when no credentials exist")
	}
	if !strings.Contains(err.summary, "no Taufinity credentials") {
		t.Errorf("summary = %q, want it to mention missing credentials", err.summary)
	}
	if !strings.Contains(err.detail, "taufinity auth login") {
		t.Errorf("detail = %q, want it to point at 'taufinity auth login'", err.detail)
	}
}

// TestStdioBridge_Non401MapsTo32000Network verifies that any non-auth
// upstream failure (429, 5xx, broken JSON, ...) collapses into the generic
// -32000 "network" code with err.Error() preserved in data.upstream.
//
// Earlier revisions parsed status codes from the transport's error string
// and emitted a dedicated -32003 rate_limited code; that regex was brittle
// against mcp-go upgrades, so we dropped it. See MEDIUM-4 in the Phase 2
// review.
func TestStdioBridge_Non401MapsTo32000Network(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()
	mu.responseStatus.Store(int32(http.StatusTooManyRequests))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsync(t, ctx, mu.url())

	frame := `{"jsonrpc":"2.0","id":99,"method":"tools/list"}` + "\n"
	_, _ = in.Write([]byte(frame))

	line := readNextFrame(t, out.(*threadSafeBuffer), 25*time.Second)
	var resp struct {
		Error struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, line)
	}
	if resp.Error.Code != errCodeNetwork {
		t.Errorf("code = %d, want %d (errCodeNetwork)", resp.Error.Code, errCodeNetwork)
	}
	if resp.Error.Data["error"] != "network" {
		t.Errorf("data.error = %v, want network", resp.Error.Data["error"])
	}
	if up, _ := resp.Error.Data["upstream"].(string); up == "" {
		t.Errorf("data.upstream missing; want raw transport error: %+v", resp.Error.Data)
	}

	_ = in.Close()
	<-done
}

func TestStdioBridge_NetworkErrorMapsTo32000(t *testing.T) {
	// Point at a closed listener to force a network error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsync(t, ctx, srv.URL+"/mcp")

	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	_, _ = in.Write([]byte(frame))

	line := readNextFrame(t, out.(*threadSafeBuffer), 5*time.Second)
	var resp struct {
		Error struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, line)
	}
	if resp.Error.Code != errCodeNetwork {
		t.Errorf("code = %d, want %d", resp.Error.Code, errCodeNetwork)
	}
	if resp.Error.Data["trace_id"] == "" || resp.Error.Data["trace_id"] == nil {
		t.Errorf("data.trace_id missing: %+v", resp.Error.Data)
	}

	_ = in.Close()
	<-done
}

func TestStdioBridge_NotificationNoResponse(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()

	// Notifications-only response: server should accept and not require a body for the test,
	// but Streamable HTTP does still POST. Ensure the bridge sends *something* upstream
	// and emits no stdout frame.
	mu.responseBody.Store(`{"jsonrpc":"2.0","id":null,"result":null}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsync(t, ctx, mu.url())

	frame := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"
	_, _ = in.Write([]byte(frame))

	// Give the bridge time to forward.
	time.Sleep(200 * time.Millisecond)

	if got := out.(*threadSafeBuffer).String(); got != "" {
		t.Errorf("expected no stdout for notification, got: %q", got)
	}

	// Verify upstream got it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.mu.Lock()
		methods := append([]string(nil), mu.methods...)
		mu.mu.Unlock()
		for _, m := range methods {
			if m == "notifications/initialized" {
				_ = in.Close()
				<-done
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.mu.Lock()
	got := append([]string(nil), mu.methods...)
	mu.mu.Unlock()
	t.Fatalf("upstream did not receive notification; got methods: %v", got)
}

func TestStdioBridge_StdinEOFExitsCleanly(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in, _, _, done := runBridgeAsync(t, ctx, mu.url())

	_ = in.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("bridge did not exit on stdin EOF within 3s")
	}
}

// TestStdioBridge_MultipleConcurrentRequests ensures multiple in-flight
// requests do not interleave their stdout writes.
func TestStdioBridge_MultipleConcurrentRequests(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsync(t, ctx, mu.url())

	// Write a few requests in quick succession.
	const n = 5
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			frame := []byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/list"}` + "\n")
			_, _ = in.Write(frame)
		}(i)
	}
	wg.Wait()

	// Read n frames, verify each is a valid full JSON object.
	deadline := time.Now().Add(5 * time.Second)
	got := 0
	tsb := out.(*threadSafeBuffer)
	for got < n && time.Now().Before(deadline) {
		s := tsb.String()
		// Each line must parse as JSON.
		scanner := bufio.NewScanner(strings.NewReader(s))
		consumed := 0
		for scanner.Scan() {
			line := scanner.Bytes()
			var v map[string]any
			if err := json.Unmarshal(line, &v); err != nil {
				t.Fatalf("interleaved frame is not valid JSON: %q (full buf: %q)", line, s)
			}
			got++
			consumed += len(line) + 1
		}
		if got >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got < n {
		t.Fatalf("got %d frames, want %d", got, n)
	}

	_ = in.Close()
	<-done
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// runBridgeAsyncWithConfig is like runBridgeAsync but accepts a fully-formed
// StdioBridgeConfig so tests can inject TokenSource, MaxFrameBytes, etc.
// The caller must populate UpstreamURL; Stdin/Stdout/Stderr are set up here.
func runBridgeAsyncWithConfig(t *testing.T, ctx context.Context, cfg StdioBridgeConfig) (
	io.WriteCloser, *threadSafeBuffer, *bytes.Buffer, chan struct{},
) {
	t.Helper()

	pr, pw := io.Pipe()
	outBuf := &threadSafeBuffer{}
	errBuf := &bytes.Buffer{}
	cfg.Stdin = pr
	cfg.Stdout = outBuf
	cfg.Stderr = errBuf
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	d := make(chan struct{})
	go func() {
		defer close(d)
		_ = RunStdioBridge(ctx, cfg)
	}()
	return pw, outBuf, errBuf, d
}

// TestStdioBridge_TokenRefreshOnEachRequest pins HIGH-1: the bridge must
// pick up rotated tokens without restarting. We rotate the TokenSource
// output mid-test and assert the bearer changes on the next upstream call.
//
// The token cache only refreshes when expiry is within tokenRefreshLeeway
// (60s), so we deliberately set short expiries that fall inside the window
// — every request triggers a TokenSource call.
func TestStdioBridge_TokenRefreshOnEachRequest(t *testing.T) {
	mu := newMockUpstream()
	defer mu.close()

	var mut sync.Mutex
	currentToken := "token-A"
	// expiresAt is set inside the leeway window so every request reloads.
	tokenSource := func(_ context.Context) (string, time.Time, error) {
		mut.Lock()
		defer mut.Unlock()
		return currentToken, time.Now().Add(10 * time.Second), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out, _, done := runBridgeAsyncWithConfig(t, ctx, StdioBridgeConfig{
		UpstreamURL: mu.url(),
		TokenSource: tokenSource,
		UserAgent:   "taufinity-cli/test (mcp-stdio)",
	})
	defer func() {
		_ = in.Close()
		<-done
	}()

	send := func(id int) {
		frame := []byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/list"}` + "\n")
		if _, err := in.Write(frame); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
		_ = readNextFrame(t, out, 5*time.Second)
	}

	// First batch: 3 requests with token-A.
	for i := 1; i <= 3; i++ {
		send(i)
	}

	// Rotate token mid-session. Real-world equivalent: user re-runs
	// `taufinity auth login`, which writes a new credentials.json.
	mut.Lock()
	currentToken = "token-B"
	mut.Unlock()

	// Next 2 requests should carry the new bearer.
	for i := 4; i <= 5; i++ {
		send(i)
	}

	mu.mu.Lock()
	auths := append([]string(nil), mu.authHeaders...)
	mu.mu.Unlock()

	if len(auths) < 5 {
		t.Fatalf("upstream saw %d auth headers, want >=5: %v", len(auths), auths)
	}

	// First three must be Bearer token-A; last two must be Bearer token-B.
	// (We tolerate extra requests — Streamable HTTP may emit a session GET
	// or similar — by only checking specific positions.)
	for i := 0; i < 3; i++ {
		if auths[i] != "Bearer token-A" {
			t.Errorf("auth[%d] = %q, want Bearer token-A", i, auths[i])
		}
	}
	// Find the first occurrence of token-B after position 2.
	sawNewToken := false
	for i := 3; i < len(auths); i++ {
		if auths[i] == "Bearer token-B" {
			sawNewToken = true
			break
		}
	}
	if !sawNewToken {
		t.Errorf("expected Bearer token-B after rotation, got: %v", auths)
	}
}

// TestStdioBridge_AgainstRealStreamableHTTPServer drives the bridge against
// mcp-go's real NewStreamableHTTPServer. The hand-rolled mockUpstream above
// covers error paths cleanly but does not exercise session-ID propagation,
// the initialize handshake, or SSE framing — a regression in any of those
// would slip past the mock-only tests. This one rounds out the protocol
// contract.
//
// HIGH-2 in the Phase 2 review.
func TestStdioBridge_AgainstRealStreamableHTTPServer(t *testing.T) {
	// Build a minimal MCP server with two tools:
	//   echo: synchronous text result.
	//   slow: sleeps briefly, then returns text. Used to prove tool/call
	//         doesn't get truncated by an early-return on the SSE stream.
	mcpServer := mcpserver.NewMCPServer("test-bridge", "0.0.0",
		mcpserver.WithToolCapabilities(true),
	)

	mcpServer.AddTool(mcp.Tool{Name: "echo"},
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("echo-ok"), nil
		},
	)
	mcpServer.AddTool(mcp.Tool{Name: "slow"},
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return mcp.NewToolResultText("slow-done"), nil
		},
	)

	streamable := mcpserver.NewStreamableHTTPServer(mcpServer,
		mcpserver.WithEndpointPath("/mcp"),
		// Stateful: server tracks Mcp-Session-Id, exercises the bridge's
		// session-id propagation through the streamable HTTP transport.
		mcpserver.WithStateful(true),
	)

	srv := httptest.NewServer(streamable)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	in, out, errBuf, done := runBridgeAsyncWithConfig(t, ctx, StdioBridgeConfig{
		UpstreamURL: srv.URL + "/mcp",
		Token:       "test-token",
		UserAgent:   "taufinity-cli/test (mcp-stdio)",
		Timeout:     10 * time.Second,
	})
	defer func() {
		_ = in.Close()
		<-done
		if t.Failed() {
			t.Logf("bridge stderr:\n%s", errBuf.String())
		}
	}()

	send := func(frame string) {
		if _, err := in.Write([]byte(frame + "\n")); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
	}

	// 1. Initialize handshake. The real Streamable HTTP server requires
	//    this before tool calls, and the response must round-trip the id.
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	initLine := readNextFrame(t, out, 5*time.Second)
	var initResp struct {
		ID     float64        `json:"id"`
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(initLine, &initResp); err != nil {
		t.Fatalf("decode init: %v: %s", err, initLine)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize error: %+v", initResp.Error)
	}
	if initResp.ID != 1 {
		t.Errorf("initialize id = %v, want 1 (id round-trip is part of the protocol contract)", initResp.ID)
	}
	if initResp.Result["protocolVersion"] == nil {
		t.Errorf("initialize result missing protocolVersion: %+v", initResp.Result)
	}

	// Per MCP spec: send notifications/initialized after init succeeds.
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// 2. tools/list — both server-registered tools come back.
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listLine := readNextFrame(t, out, 5*time.Second)
	var listResp struct {
		ID     float64 `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listLine, &listResp); err != nil {
		t.Fatalf("decode tools/list: %v: %s", err, listLine)
	}
	if listResp.ID != 2 {
		t.Errorf("tools/list id = %v, want 2", listResp.ID)
	}
	names := map[string]bool{}
	for _, tool := range listResp.Result.Tools {
		names[tool.Name] = true
	}
	if !names["echo"] || !names["slow"] {
		t.Errorf("tools/list missing expected tools, got: %+v", listResp.Result.Tools)
	}

	// 3. tools/call → echo (synchronous).
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	echoLine := readNextFrame(t, out, 5*time.Second)
	if !strings.Contains(string(echoLine), "echo-ok") {
		t.Errorf("echo result missing 'echo-ok': %s", echoLine)
	}

	// 4. tools/call → slow (handler sleeps before returning). Proves the
	//    bridge waits for the tool to finish rather than racing the SSE
	//    response.
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	slowLine := readNextFrame(t, out, 5*time.Second)
	if !strings.Contains(string(slowLine), "slow-done") {
		t.Errorf("slow result missing 'slow-done': %s", slowLine)
	}
	var slowResp struct {
		ID float64 `json:"id"`
	}
	if err := json.Unmarshal(slowLine, &slowResp); err == nil && slowResp.ID != 4 {
		t.Errorf("slow result id = %v, want 4 (id round-trip across multiple in-flight requests)", slowResp.ID)
	}
}
