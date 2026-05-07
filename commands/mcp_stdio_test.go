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
func readNextFrame(t *testing.T, buf *threadSafeBuffer, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := buf.String()
		idx := strings.IndexByte(s, '\n')
		if idx >= 0 {
			line := []byte(s[:idx])
			// drain that much from the buffer
			rest := s[idx+1:]
			buf.mu.Lock()
			buf.buf.Reset()
			buf.buf.WriteString(rest)
			buf.mu.Unlock()
			return line
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no frame on stdout within %s; have: %q", timeout, buf.String())
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
	if resp.Error.Data["error"] != "auth_failed" {
		t.Errorf("data.error = %v, want auth_failed", resp.Error.Data["error"])
	}
	if hint, _ := resp.Error.Data["hint"].(string); !strings.Contains(hint, "taufinity auth login") {
		t.Errorf("data.hint = %v, want hint to contain 'taufinity auth login'", resp.Error.Data["hint"])
	}

	_ = in.Close()
	<-done
}

func TestStdioBridge_RateLimitedMapsTo32003(t *testing.T) {
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
	if resp.Error.Code != errCodeRateLimited {
		t.Errorf("code = %d, want %d", resp.Error.Code, errCodeRateLimited)
	}
	if resp.Error.Data["error"] != "rate_limited" {
		t.Errorf("data.error = %v, want rate_limited", resp.Error.Data["error"])
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
