package commands

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	flagMCPStdioTimeout       time.Duration
	flagMCPStdioMaxFrameBytes int
)

// defaultMaxFrameBytes is the default max stdin frame size (16 MiB).
// Large tools/list responses or rich tool results can exceed the bufio
// default of 64 KiB.
const defaultMaxFrameBytes = 16 * 1024 * 1024

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

To pin a specific organization (e.g. when your global CLI config points to a
different org), use the --org flag:

  {
    "mcpServers": {
      "taufinity-acme": {
        "command": "taufinity",
        "args": ["--org", "3", "mcp", "stdio"]
      }
    }
  }

The --org value is sent as X-Organization-ID on every request, overriding the
organization embedded in the JWT.

All log output goes to stderr; JSON-RPC frames go to stdout.`,
	RunE: runMCPStdio,
	Annotations: map[string]string{
		// Suppress the staleness check and warning while running as an MCP
		// bridge: the process is long-running, network-noisy chatter is
		// unwanted, and the client doesn't expect informational stderr.
		"suppress-update-warning": "true",
	},
}

func init() {
	mcpCmd.AddCommand(mcpStdioCmd)
	mcpStdioCmd.Flags().DurationVar(&flagMCPStdioTimeout, "timeout", 300*time.Second, "Per-request HTTP timeout (BigQuery-backed tools can run long)")
	mcpStdioCmd.Flags().IntVar(&flagMCPStdioMaxFrameBytes, "max-frame-bytes", defaultMaxFrameBytes, "Maximum size of a single JSON-RPC frame read from stdin (bytes); raise if tools/list or rich results are truncated")
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
//
// We deliberately collapse rate-limited/5xx into errCodeNetwork rather than
// pattern-matching transport error strings (see mapTransportError). The
// upstream message is preserved in error data.upstream.
const (
	errCodeNetwork    = -32000
	errCodeAuthFailed = -32001
	errCodeMethodNF   = -32601
)

// authFailedMessage is the human-readable text MCP clients display when the
// bridge can't authenticate to Studio — either at startup (degraded bridge)
// or mid-session (upstream 401). One canonical phrasing keeps the user
// experience consistent and stops programmatic clients from having to
// branch on hint text.
const authFailedMessage = "Taufinity authentication required — run 'taufinity auth login' in a terminal, then restart this MCP server in your client"

// authFailedHint is the same instruction, surfaced via error.data.hint for
// programmatic clients. Kept identical-in-meaning to authFailedMessage so a
// client that only renders one of the two still tells the user what to do.
const authFailedHint = "run 'taufinity auth login' in a terminal, then restart this MCP server in your client"

func runMCPStdio(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Probe credentials at startup. If they are missing or expired we do NOT
	// exit — that leaves the MCP client showing a useless "server disconnected"
	// popup with no actionable message. Instead, run a degraded bridge that
	// stays alive and replies to every JSON-RPC request with an auth_failed
	// error frame the client can surface to the user.
	if startupErr := probeStartupAuth(); startupErr != nil {
		fmt.Fprintf(os.Stderr, "[taufinity mcp stdio] %s — %s\n", startupErr.summary, startupErr.detail)
		fmt.Fprintf(os.Stderr, "[taufinity mcp stdio] running in degraded mode; every request will return auth_failed until you re-authenticate\n")
		return runDegradedBridge(ctx, os.Stdin, os.Stdout, os.Stderr, startupErr)
	}

	upstreamURL := strings.TrimRight(GetAPIURL(), "/") + "/mcp"
	userAgent := fmt.Sprintf("taufinity-cli/%s (mcp-stdio)", Version)

	return RunStdioBridge(ctx, StdioBridgeConfig{
		UpstreamURL:   upstreamURL,
		TokenSource:   defaultTokenSource,
		OrgID:         flagOrg,
		UserAgent:     userAgent,
		Timeout:       flagMCPStdioTimeout,
		MaxFrameBytes: flagMCPStdioMaxFrameBytes,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	})
}

// startupAuthError captures why the credential probe failed at bridge
// startup. It carries enough detail to render a useful JSON-RPC error frame
// from the degraded bridge (summary → Message, detail/expiresAt → Data).
type startupAuthError struct {
	summary   string    // short user-facing reason ("Taufinity credentials expired")
	detail    string    // longer technical detail (e.g. "token expired at 2026-05-12T17:45:23Z")
	expiresAt time.Time // zero if unknown
}

func (e *startupAuthError) Error() string {
	if e.detail != "" {
		return e.summary + ": " + e.detail
	}
	return e.summary
}

// probeStartupAuth checks for usable credentials. Returns nil when the bridge
// can start normally, or a *startupAuthError describing the problem.
func probeStartupAuth() *startupAuthError {
	if !auth.HasCredentials() {
		return &startupAuthError{
			summary: "no Taufinity credentials found",
			detail:  "no credentials file on disk; run 'taufinity auth login' to sign in",
		}
	}
	creds, err := auth.LoadCredentials()
	if err != nil {
		return &startupAuthError{
			summary: "could not read Taufinity credentials",
			detail:  err.Error(),
		}
	}
	if _, err := creds.GetValidToken(); err != nil {
		return &startupAuthError{
			summary:   "Taufinity credentials expired",
			detail:    err.Error(),
			expiresAt: creds.ExpiresAt,
		}
	}
	return nil
}

// runDegradedBridge reads JSON-RPC frames from stdin and replies to every
// request with an auth_failed error. It is used when the startup credential
// probe fails. Notifications are dropped silently (JSON-RPC notifications
// have no id and expect no reply).
//
// The point is to keep the subprocess alive long enough for the MCP client
// to send `initialize` and receive a real, surface-able error — instead of
// the generic "server disconnected" the client shows when we exit before
// the handshake completes.
func runDegradedBridge(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, authErr *startupAuthError) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), defaultMaxFrameBytes)

	var writeMu sync.Mutex
	writeFrame := func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		writeMu.Lock()
		defer writeMu.Unlock()
		_, werr := stdout.Write(data)
		return werr
	}

	// Message prefixes the canonical auth-failed text with the specific reason
	// (e.g. "Taufinity credentials expired — ..."), so the client surfaces both
	// the diagnosis and the fix in one line.
	message := authErr.summary + " — " + authFailedHint
	// detail carries the GetValidToken error string for context. That string
	// is only ever "token expired at <RFC3339 timestamp>" (see
	// internal/auth/credentials.go GetValidToken) — no token bytes, no path.
	// Anyone editing GetValidToken: keep it that way.
	baseData := map[string]any{
		"error":  "auth_failed",
		"detail": authErr.detail,
		"hint":   authFailedHint,
	}
	if !authErr.expiresAt.IsZero() {
		baseData["expired_at"] = authErr.expiresAt.UTC().Format(time.RFC3339)
	}

	// Run scan in a goroutine so ctx cancellation unblocks the main loop
	// promptly. NOTE: on ctx cancel the scanner goroutine stays parked
	// inside scanner.Scan() until stdin closes (real stdio: when the OS
	// closes the fd at process exit; tests: when the caller closes stdinW).
	// This is acceptable because the bridge runs as a top-level process.
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
		// bufio.Scanner.Err() returns nil on clean EOF — pass it through and
		// let the main loop return nil. No io.EOF mapping needed.
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
			var frame jsonRPCFrame
			if err := json.Unmarshal(trimmed, &frame); err != nil {
				fmt.Fprintf(stderr, "[taufinity mcp stdio] invalid JSON from stdin: %v\n", err)
				continue
			}
			// Notification: no id field, or id explicitly null (JSON-RPC 2.0
			// treats both as notifications in MCP practice; spec allows null
			// requests but no real MCP server uses them). TrimSpace tolerates
			// whitespace inside the raw id value.
			if len(frame.ID) == 0 || bytes.Equal(bytes.TrimSpace(frame.ID), []byte("null")) {
				continue
			}
			var id mcp.RequestId
			if err := json.Unmarshal(frame.ID, &id); err != nil {
				fmt.Fprintf(stderr, "[taufinity mcp stdio] invalid request id: %v\n", err)
				continue
			}

			data := make(map[string]any, len(baseData)+1)
			for k, v := range baseData {
				data[k] = v
			}
			data["trace_id"] = newTraceID()

			resp := struct {
				JSONRPC string                  `json:"jsonrpc"`
				ID      mcp.RequestId           `json:"id"`
				Error   mcp.JSONRPCErrorDetails `json:"error"`
			}{
				JSONRPC: "2.0",
				ID:      id,
				Error: mcp.JSONRPCErrorDetails{
					Code:    errCodeAuthFailed,
					Message: message,
					Data:    data,
				},
			}
			if err := writeFrame(resp); err != nil {
				return err
			}
		}
	}
}

// defaultTokenSource reloads credentials from disk on every call so the
// bridge picks up tokens rotated by an out-of-process re-login (the user
// running `taufinity auth login` again, or a future in-process refresh).
// Long-lived Claude Desktop subprocesses must NOT cache the bearer that
// was valid at startup; that is what HIGH-1 was about.
func defaultTokenSource(_ context.Context) (token string, expiresAt time.Time, err error) {
	creds, err := auth.LoadCredentials()
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := creds.GetValidToken()
	if err != nil {
		return "", creds.ExpiresAt, err
	}
	return tok, creds.ExpiresAt, nil
}

// TokenSource produces a bearer token for the upstream /mcp endpoint.
// It is called once per outbound HTTP request (via WithHTTPHeaderFunc),
// with results cached in-memory until the token is within tokenRefreshLeeway
// of expiry. Implementations should be cheap (single file read) but safe
// to call concurrently. Returning an error suppresses the Authorization
// header for that request, surfacing a clear upstream 401 to the client
// rather than silently sending a stale bearer.
type TokenSource func(ctx context.Context) (token string, expiresAt time.Time, err error)

// StdioBridgeConfig configures the stdio MCP bridge.
//
// Either TokenSource or Token may be set. TokenSource is preferred — it
// is called per-request so a token rotated outside the process (e.g. user
// re-running `taufinity auth login`) is picked up without restarting the
// bridge. Token is a static fallback for tests and embedded callers; it
// never expires for the bridge's purposes.
type StdioBridgeConfig struct {
	UpstreamURL   string
	TokenSource   TokenSource
	Token         string // static fallback; ignored if TokenSource is set
	OrgID         string // if set, sends X-Organization-ID on every request
	UserAgent     string
	Timeout       time.Duration
	MaxFrameBytes int // 0 → defaultMaxFrameBytes
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
}

// tokenRefreshLeeway is how close to expiry we re-read credentials from disk.
// Anything under this threshold triggers a refresh on the next request; above
// it, the cached token is reused to keep the per-request overhead at one map
// allocation.
const tokenRefreshLeeway = 60 * time.Second

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
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = defaultMaxFrameBytes
	}

	br := &bridge{
		stdout:        cfg.Stdout,
		stderr:        cfg.Stderr,
		timeout:       cfg.Timeout,
		maxFrameBytes: cfg.MaxFrameBytes,
		userAgent:     cfg.UserAgent,
		orgID:         cfg.OrgID,
		tokenSource:   cfg.TokenSource,
		staticToken:   cfg.Token,
	}

	tr, err := transport.NewStreamableHTTP(
		cfg.UpstreamURL,
		// All headers — Authorization, User-Agent, etc — flow through this
		// per-request callback. We never call WithHTTPHeaders, because that
		// would freeze the bearer at bridge startup; Claude Desktop keeps the
		// subprocess alive for hours/days, and the original token will expire
		// mid-session. See HIGH-1 in the Phase 2 review.
		transport.WithHTTPHeaderFunc(br.requestHeaders),
		transport.WithHTTPTimeout(cfg.Timeout),
	)
	if err != nil {
		return fmt.Errorf("create upstream transport: %w", err)
	}

	if err := tr.Start(ctx); err != nil {
		return fmt.Errorf("start upstream transport: %w", err)
	}

	br.transport = tr

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
	transport     transport.Interface
	stdout        io.Writer
	stderr        io.Writer
	timeout       time.Duration
	maxFrameBytes int
	userAgent     string
	orgID         string // forwarded as X-Organization-ID if non-empty

	tokenSource TokenSource
	staticToken string

	tokenMu        sync.Mutex // guards cachedToken/cachedExpiresAt/cachedHeaders
	cachedToken    string
	cachedExpires  time.Time
	cachedHeaders  map[string]string
	tokenLoadCount int64 // observability: how often we hit the TokenSource

	writeMu sync.Mutex // guards writes to stdout
	wg      sync.WaitGroup
}

// requestHeaders is invoked by the StreamableHTTP transport for every
// outbound request (POST /mcp, the GET listening connection, session
// terminate, etc). It returns the headers map for that request — fresh
// bearer token, User-Agent, and any future per-request metadata.
//
// Caching policy:
//   - Static token (cfg.Token, no TokenSource) → never refreshes.
//   - TokenSource → refresh on first call, or when current token expires
//     within tokenRefreshLeeway. We do NOT block other requests during the
//     refresh; the mutex is held only while reading the cache or swapping it.
//   - On TokenSource error, log to stderr and reuse the previous cached
//     headers if any — better to let upstream return 401 with a clear
//     trace_id than to silently drop the Authorization header.
func (b *bridge) requestHeaders(ctx context.Context) map[string]string {
	b.tokenMu.Lock()
	cached := b.cachedHeaders
	cachedExpires := b.cachedExpires
	b.tokenMu.Unlock()

	// Static-token mode (tests, embedded callers).
	if b.tokenSource == nil {
		if cached != nil {
			return cached
		}
		h := b.buildHeaders(b.staticToken)
		b.tokenMu.Lock()
		b.cachedHeaders = h
		b.tokenMu.Unlock()
		return h
	}

	// Cache hit: token still has comfortable headroom.
	if cached != nil && !cachedExpires.IsZero() && time.Until(cachedExpires) > tokenRefreshLeeway {
		return cached
	}

	token, expiresAt, err := b.tokenSource(ctx)
	b.tokenMu.Lock()
	b.tokenLoadCount++
	if err != nil {
		// Reuse previous headers if any. The next request will retry.
		// If we have no cache yet, return headers without Authorization
		// so the upstream returns 401 — clear failure mode.
		b.logf("token refresh failed (request will use stale or no bearer): %v", err)
		if b.cachedHeaders != nil {
			result := b.cachedHeaders
			b.tokenMu.Unlock()
			return result
		}
		h := b.buildHeaders("")
		b.cachedHeaders = h
		b.tokenMu.Unlock()
		return h
	}

	h := b.buildHeaders(token)
	b.cachedToken = token
	b.cachedExpires = expiresAt
	b.cachedHeaders = h
	b.tokenMu.Unlock()
	return h
}

// buildHeaders constructs the per-request header map. token may be empty
// if the source failed and we have no prior cache.
func (b *bridge) buildHeaders(token string) map[string]string {
	h := make(map[string]string, 3)
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	if b.userAgent != "" {
		h["User-Agent"] = b.userAgent
	}
	if b.orgID != "" {
		h["X-Organization-ID"] = b.orgID
	}
	return h
}

// readLoop reads JSON-RPC frames (one per line) from r and dispatches them.
func (b *bridge) readLoop(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	max := b.maxFrameBytes
	if max <= 0 {
		max = defaultMaxFrameBytes
	}
	// Start with a 1 MiB buffer; bufio grows up to max as needed.
	initial := 1024 * 1024
	if initial > max {
		initial = max
	}
	buf := make([]byte, 0, initial)
	scanner.Buffer(buf, max)

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
			if errors.Is(err, bufio.ErrTooLong) {
				b.logf("stdin frame exceeded --max-frame-bytes (%d); raise the flag to allow larger JSON-RPC frames", max)
				return fmt.Errorf("stdin frame exceeded --max-frame-bytes=%d: %w", max, err)
			}
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
//
// We only special-case auth — everything else collapses into -32000 "network".
// Earlier revisions parsed status codes out of the transport's error string
// (e.g. "request failed with status 429: ..."), but that regex is fragile
// against mcp-go version changes. The cost of losing the rate_limited / 5xx
// distinction is small: the raw upstream error is preserved in data.upstream
// so a client (or operator reading the trace) can still see what happened.
//
// MEDIUM-4 in the Phase 2 review: keep this surface small.
//
// Note: data is never the raw bearer token.
func mapTransportError(err error) (int, string, map[string]any) {
	traceID := newTraceID()

	if errors.Is(err, transport.ErrUnauthorized) {
		return errCodeAuthFailed, authFailedMessage, map[string]any{
			"error":    "auth_failed",
			"hint":     authFailedHint,
			"trace_id": traceID,
		}
	}

	return errCodeNetwork, "network", map[string]any{
		"error":    "network",
		"upstream": err.Error(),
		"trace_id": traceID,
	}
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
