package commands

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/auth"
)

var (
	flagMCPStdioTimeout time.Duration
)

var mcpStdioCmd = &cobra.Command{
	Use:   "stdio",
	Short: "Bridge stdio MCP clients to Studio's remote /mcp endpoint",
	Long: `Run a stdio MCP bridge that forwards JSON-RPC frames over stdio
to Studio's remote /mcp HTTP endpoint.

Use this for stdio-only MCP clients (older Claude Desktop versions,
mcp-inspector, custom clients). The bridge is a pure passthrough — it
does not register tools locally.

Authentication uses your existing CLI credentials (from 'taufinity auth login').
The remote URL is resolved from --api-url, $TAUFINITY_API_URL, the CLI config,
or defaults to https://studio.taufinity.io.

Configure Claude Desktop by adding to its mcpServers config:

  {
    "mcpServers": {
      "taufinity-studio": {
        "command": "taufinity",
        "args": ["mcp", "stdio"]
      }
    }
  }

All log output goes to stderr; JSON-RPC frames go to stdout.`,
	RunE: runMCPStdio,
}

func init() {
	mcpCmd.AddCommand(mcpStdioCmd)
	mcpStdioCmd.Flags().DurationVar(&flagMCPStdioTimeout, "timeout", 300*time.Second, "Per-request HTTP timeout (BigQuery-backed tools can run long)")
}

// jsonRPCFrame is a minimal envelope used to peek at id/method
// without losing the original payload during decode/encode.
type jsonRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSON-RPC error codes used by the bridge.
const (
	errCodeNetwork     = -32000
	errCodeAuthFailed  = -32001
	errCodeRateLimited = -32003
	errCodeMethodNF    = -32601
)

func runMCPStdio(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}
	creds, err := auth.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	token, err := creds.GetValidToken()
	if err != nil {
		return fmt.Errorf("run 'taufinity auth login' to re-authenticate: %w", err)
	}

	upstreamURL := strings.TrimRight(GetAPIURL(), "/") + "/mcp"
	userAgent := fmt.Sprintf("taufinity-cli/%s (mcp-stdio)", Version)

	return RunStdioBridge(ctx, StdioBridgeConfig{
		UpstreamURL: upstreamURL,
		Token:       token,
		UserAgent:   userAgent,
		Timeout:     flagMCPStdioTimeout,
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	})
}

// StdioBridgeConfig configures the stdio MCP bridge.
type StdioBridgeConfig struct {
	UpstreamURL string
	Token       string
	UserAgent   string
	Timeout     time.Duration
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// RunStdioBridge runs the stdio bridge against the configured upstream.
// It blocks until ctx is canceled, stdin reaches EOF, or a fatal error occurs.
func RunStdioBridge(ctx context.Context, cfg StdioBridgeConfig) error {
	if cfg.UpstreamURL == "" {
		return fmt.Errorf("upstream URL required")
	}
	if cfg.Stdin == nil {
		cfg.Stdin = os.Stdin
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 300 * time.Second
	}

	headers := map[string]string{}
	if cfg.Token != "" {
		headers["Authorization"] = "Bearer " + cfg.Token
	}
	if cfg.UserAgent != "" {
		headers["User-Agent"] = cfg.UserAgent
	}

	tr, err := transport.NewStreamableHTTP(
		cfg.UpstreamURL,
		transport.WithHTTPHeaders(headers),
		transport.WithHTTPTimeout(cfg.Timeout),
	)
	if err != nil {
		return fmt.Errorf("create upstream transport: %w", err)
	}

	if err := tr.Start(ctx); err != nil {
		return fmt.Errorf("start upstream transport: %w", err)
	}

	br := &bridge{
		transport: tr,
		stdout:    cfg.Stdout,
		stderr:    cfg.Stderr,
		timeout:   cfg.Timeout,
	}

	// Server -> client notifications: write to stdout.
	tr.SetNotificationHandler(br.handleServerNotification)
	// Server -> client requests (sampling/elicitation/roots): explicit reject.
	tr.SetRequestHandler(br.handleServerRequest)

	br.logf("MCP stdio bridge connected to %s", cfg.UpstreamURL)

	// Read frames from stdin until EOF or ctx cancel.
	readErr := br.readLoop(ctx, cfg.Stdin)

	// Drain in-flight before close.
	br.wg.Wait()

	if cerr := tr.Close(); cerr != nil {
		br.logf("transport close error: %v", cerr)
	}

	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, context.Canceled) {
		return readErr
	}
	return nil
}

// bridge holds the runtime state for an active stdio bridge.
type bridge struct {
	transport transport.Interface
	stdout    io.Writer
	stderr    io.Writer
	timeout   time.Duration

	writeMu sync.Mutex // guards writes to stdout
	wg      sync.WaitGroup
}

// readLoop reads JSON-RPC frames (one per line) from r and dispatches them.
func (b *bridge) readLoop(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// Allow large frames: tools/list responses or rich tool results
	// can easily exceed the default 64 KiB.
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	// Run the scan in its own goroutine so ctx cancel unblocks us.
	lineCh := make(chan []byte)
	errCh := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lineCh <- line:
			case <-ctx.Done():
				return
			}
		}
		errCh <- scanner.Err()
		close(lineCh)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case line, ok := <-lineCh:
			if !ok {
				return nil
			}
			trimmed := stripWhitespace(line)
			if len(trimmed) == 0 {
				continue
			}
			b.dispatchFrame(ctx, trimmed)
		}
	}
}

func stripWhitespace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// dispatchFrame routes a single inbound JSON-RPC frame to the upstream.
// Requests get a goroutine each so multiple in-flight requests are allowed.
func (b *bridge) dispatchFrame(ctx context.Context, raw []byte) {
	var frame jsonRPCFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		b.logf("invalid JSON from stdin: %v", err)
		// We can't reply — no id available.
		return
	}

	// Notification: no id field (or explicit null id is treated as request per JSON-RPC 2.0,
	// but in practice MCP uses missing id to mean notification).
	isNotification := len(frame.ID) == 0 || string(frame.ID) == "null"

	if isNotification {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.forwardNotification(ctx, frame)
		}()
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.forwardRequest(ctx, raw, frame)
	}()
}

// forwardRequest sends a client request to the upstream and writes the response to stdout.
func (b *bridge) forwardRequest(ctx context.Context, raw []byte, frame jsonRPCFrame) {
	// Decode the inbound frame into the transport's request type, preserving id and params.
	req := transport.JSONRPCRequest{
		JSONRPC: frame.JSONRPC,
		Method:  frame.Method,
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}
	if err := json.Unmarshal(frame.ID, &req.ID); err != nil {
		b.logf("invalid request id: %v", err)
		return
	}
	if len(frame.Params) > 0 {
		var params any
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			b.writeErrorResponse(req.ID, errCodeNetwork, "invalid params", map[string]any{"detail": err.Error()})
			return
		}
		req.Params = params
	}

	reqCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	resp, err := b.transport.SendRequest(reqCtx, req)
	if err != nil {
		code, msg, data := mapTransportError(err)
		b.writeErrorResponse(req.ID, code, msg, data)
		return
	}

	if resp == nil {
		b.writeErrorResponse(req.ID, errCodeNetwork, "empty response from upstream", map[string]any{
			"upstream": "request returned nil",
			"trace_id": newTraceID(),
		})
		return
	}

	// Re-marshal: ensure id is preserved and that the original wire shape (jsonrpc/id/result|error)
	// is what we hand back to stdout.
	out := struct {
		JSONRPC string                       `json:"jsonrpc"`
		ID      mcp.RequestId                `json:"id"`
		Result  json.RawMessage              `json:"result,omitempty"`
		Error   *mcp.JSONRPCErrorDetails     `json:"error,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  resp.Result,
		Error:   resp.Error,
	}
	if out.Result == nil && out.Error == nil {
		// JSON-RPC requires at least one of result/error. Surface as empty result.
		out.Result = json.RawMessage(`null`)
	}

	if err := b.writeFrame(out); err != nil {
		b.logf("write response: %v", err)
	}

	_ = raw // raw is intentionally not echoed; we use the structured response.
}

// forwardNotification sends a client notification upstream. No reply expected.
func (b *bridge) forwardNotification(ctx context.Context, frame jsonRPCFrame) {
	notif := mcp.JSONRPCNotification{
		JSONRPC: "2.0",
	}
	notif.Method = frame.Method
	if len(frame.Params) > 0 {
		// Notification.Params has a custom UnmarshalJSON that handles arbitrary maps.
		if err := json.Unmarshal(frame.Params, &notif.Params); err != nil {
			b.logf("notification params decode (%s): %v", frame.Method, err)
		}
	}

	notifCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	if err := b.transport.SendNotification(notifCtx, notif); err != nil {
		b.logf("send notification %s: %v", frame.Method, err)
	}
}

// handleServerNotification is invoked by the transport when the upstream pushes
// a notification. We forward it to stdout verbatim.
func (b *bridge) handleServerNotification(notif mcp.JSONRPCNotification) {
	if err := b.writeFrame(notif); err != nil {
		b.logf("forward server notification: %v", err)
	}
}

// handleServerRequest is invoked by the transport when the upstream sends a
// request to the client (sampling, elicitation, roots/list). v1 explicitly
// rejects with -32601 method not found.
func (b *bridge) handleServerRequest(_ context.Context, req transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	return &transport.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &mcp.JSONRPCErrorDetails{
			Code:    errCodeMethodNF,
			Message: fmt.Sprintf("server-to-client method %q not supported by taufinity stdio bridge v1", req.Method),
		},
	}, nil
}

// writeFrame serializes v as a single JSON line on stdout under the write mutex.
func (b *bridge) writeFrame(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	data = append(data, '\n')

	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err = b.stdout.Write(data)
	return err
}

// writeErrorResponse emits a JSON-RPC error frame. The variadic form accepts:
// (id, code, message) or (id, code, message, dataMap).
func (b *bridge) writeErrorResponse(id mcp.RequestId, args ...any) {
	if len(args) < 2 {
		return
	}
	code, _ := args[0].(int)
	message, _ := args[1].(string)
	var data any
	if len(args) >= 3 {
		data = args[2]
	}

	frame := struct {
		JSONRPC string                   `json:"jsonrpc"`
		ID      mcp.RequestId            `json:"id"`
		Error   mcp.JSONRPCErrorDetails  `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Error: mcp.JSONRPCErrorDetails{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	if err := b.writeFrame(frame); err != nil {
		b.logf("write error response: %v", err)
	}
}

// mapTransportError turns a transport-layer error into a (code, message, data) triple.
// Note: data is never the raw bearer token.
func mapTransportError(err error) (int, string, map[string]any) {
	traceID := newTraceID()

	if errors.Is(err, transport.ErrUnauthorized) {
		return errCodeAuthFailed, "auth_failed", map[string]any{
			"error":    "auth_failed",
			"hint":     "run taufinity auth login",
			"trace_id": traceID,
		}
	}

	if status, ok := statusCodeFromError(err); ok {
		switch {
		case status == http.StatusUnauthorized:
			return errCodeAuthFailed, "auth_failed", map[string]any{
				"error":    "auth_failed",
				"hint":     "run taufinity auth login",
				"trace_id": traceID,
			}
		case status == http.StatusTooManyRequests:
			return errCodeRateLimited, "rate_limited", map[string]any{
				"error":    "rate_limited",
				"trace_id": traceID,
			}
		case status >= 500 && status < 600:
			return errCodeNetwork, "network", map[string]any{
				"error":    "network",
				"upstream": fmt.Sprintf("status %d", status),
				"trace_id": traceID,
			}
		}
	}

	return errCodeNetwork, "network", map[string]any{
		"error":    "network",
		"upstream": err.Error(),
		"trace_id": traceID,
	}
}

// statusCodeFromError tries to extract an HTTP status code from the
// transport's error message format ("request failed with status %d: ...").
func statusCodeFromError(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	s := err.Error()
	const marker = "status "
	idx := strings.Index(s, marker)
	if idx < 0 {
		return 0, false
	}
	rest := s[idx+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n := 0
	for i := 0; i < end; i++ {
		n = n*10 + int(rest[i]-'0')
	}
	return n, true
}

// newTraceID generates a short hex trace id for error correlation.
func newTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

func (b *bridge) logf(format string, args ...any) {
	fmt.Fprintf(b.stderr, "[taufinity mcp stdio] "+format+"\n", args...)
}
