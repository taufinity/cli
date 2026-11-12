// Package httpclient provides a robust HTTP client with retry logic,
// exponential backoff, configurable timeouts, and structured logging.
package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrResponseTooLarge is returned when a response body passes Config.MaxResponseBytes.
var ErrResponseTooLarge = errors.New("response body exceeds the size limit")

// Config configures the resilient HTTP client
type Config struct {
	// Timeout is the total request timeout (default: 60s)
	Timeout time.Duration

	// MaxRetries is the maximum number of retry attempts. Zero means NO retry:
	// the request is sent once. DefaultConfig() sets 3, so a client that wants
	// the default starts from DefaultConfig().
	MaxRetries int

	// NoTransportRetry stops retrying on transport errors (timeouts, EOF,
	// connection resets); only RetryStatusCodes still retry. For uploads that
	// must never be re-sent on the client's own initiative.
	NoTransportRetry bool

	// HonorRetryAfter makes a retry after a retryable status wait for the
	// server's Retry-After header (seconds or HTTP date) instead of the
	// exponential backoff. Without the header InitialBackoff is used; the wait
	// is never longer than MaxBackoff.
	HonorRetryAfter bool

	// MaxResponseBytes caps the response body (0 = unlimited). A larger body
	// is not buffered further: Do fails with ErrResponseTooLarge.
	MaxResponseBytes int64

	// ReturnFinalStatus returns the last retryable response as a normal
	// Response with a nil error once the retries are used up, so the caller
	// can read its status, headers and body.
	ReturnFinalStatus bool

	// InitialBackoff is the initial backoff duration (default: 2s)
	InitialBackoff time.Duration

	// MaxBackoff is the maximum backoff duration (default: 30s)
	MaxBackoff time.Duration

	// BackoffMultiplier is the backoff multiplier (default: 2.0)
	BackoffMultiplier float64

	// JitterFraction adds randomness to backoff (default: 0.1 = 10%)
	JitterFraction float64

	// RetryStatusCodes are HTTP status codes that should trigger a retry
	// Default: 429, 500, 502, 503, 504, 529
	RetryStatusCodes []int

	// LogRequests enables request/response logging at debug level
	LogRequests bool

	// Transport overrides the default http.Transport. Useful for testing
	// (VCR record/replay), proxying, or custom TLS configuration.
	Transport http.RoundTripper
}

// DefaultConfig returns sensible defaults for the HTTP client
func DefaultConfig() Config {
	return Config{
		Timeout:           60 * time.Second,
		MaxRetries:        3,
		InitialBackoff:    2 * time.Second,
		MaxBackoff:        30 * time.Second,
		BackoffMultiplier: 2.0,
		JitterFraction:    0.1,
		RetryStatusCodes:  []int{429, 500, 502, 503, 504, 529},
		LogRequests:       false,
	}
}

// Client is a resilient HTTP client with retry and backoff
type Client struct {
	client *http.Client
	config Config
}

// New creates a new resilient HTTP client with the given configuration
func New(cfg Config) *Client {
	// Apply defaults for zero values
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.BackoffMultiplier == 0 {
		cfg.BackoffMultiplier = 2.0
	}
	if len(cfg.RetryStatusCodes) == 0 {
		cfg.RetryStatusCodes = []int{429, 500, 502, 503, 504, 529}
	}

	httpClient := &http.Client{
		Timeout: cfg.Timeout,
	}
	if cfg.Transport != nil {
		httpClient.Transport = cfg.Transport
	}

	return &Client{
		client: httpClient,
		config: cfg,
	}
}

// NewWithDefaults creates a new client with default configuration
func NewWithDefaults() *Client {
	return New(DefaultConfig())
}

// Response wraps http.Response with the body already read
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Do performs an HTTP request with retry logic
func (c *Client) Do(req *http.Request) (*Response, error) {
	return c.DoWithContext(req.Context(), req)
}

// DoWithContext performs an HTTP request with retry logic and context
func (c *Client) DoWithContext(ctx context.Context, req *http.Request) (*Response, error) {
	var lastErr error
	var lastStatusCode int
	var lastResponseBody []byte // Store last response body for error reporting
	var lastHeaders http.Header
	var bodyBytes []byte

	// Read body once if present (for retries)
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}
	}

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		// Check context before each attempt
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Apply backoff for retries
		if attempt > 0 {
			backoff := c.calculateBackoff(attempt - 1)
			if c.config.HonorRetryAfter {
				backoff = RetryAfter(lastHeaders, c.config.InitialBackoff, c.config.MaxBackoff)
			}
			slog.Debug("HTTP retry backoff",
				"attempt", attempt,
				"max_attempts", c.config.MaxRetries+1,
				"backoff", backoff,
				"url", req.URL.Host+req.URL.Path,
			)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		// Reset body for retry
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		// Clone request with context
		reqWithCtx := req.Clone(ctx)

		if c.config.LogRequests {
			slog.Debug("HTTP request",
				"method", req.Method,
				"url", req.URL.String(),
				"attempt", attempt+1,
			)
		}

		resp, err := c.client.Do(reqWithCtx)
		if err != nil {
			// An error that declares itself final (e.g. Cloudflare Access
			// turning the request away) is returned as-is: retrying only
			// repeats it, with backoff the user sits through.
			var final interface{ NonRetryable() bool }
			if errors.As(err, &final) && final.NonRetryable() {
				return nil, err
			}
			lastErr = err
			lastHeaders, lastResponseBody = nil, nil
			if !c.config.NoTransportRetry && c.isRetryableError(err, 0) {
				slog.Warn("HTTP request failed, will retry",
					"error", err,
					"attempt", attempt+1,
					"max_attempts", c.config.MaxRetries+1,
				)
				continue
			}
			return nil, fmt.Errorf("request failed: %w", err)
		}

		// Read response body
		var body []byte
		if c.config.MaxResponseBytes > 0 {
			body, err = io.ReadAll(io.LimitReader(resp.Body, c.config.MaxResponseBytes+1))
			if err == nil && int64(len(body)) > c.config.MaxResponseBytes {
				resp.Body.Close()
				return nil, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, c.config.MaxResponseBytes)
			}
		} else {
			body, err = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		if err != nil {
			if c.config.NoTransportRetry {
				// a timeout or reset in the middle of the answer: the request was
				// already sent, so it is never sent again
				return nil, fmt.Errorf("request failed: %w", err)
			}
			lastErr = err
			lastStatusCode = resp.StatusCode
			continue
		}

		lastStatusCode = resp.StatusCode

		// Check if we should retry based on status code
		if c.isRetryableStatusCode(resp.StatusCode) {
			lastResponseBody = body
			lastHeaders = resp.Header
			// Truncate body for logging (max 500 chars)
			bodyPreview := string(body)
			if len(bodyPreview) > 500 {
				bodyPreview = bodyPreview[:500] + "..."
			}
			lastErr = fmt.Errorf("received retryable status code %d: %s", resp.StatusCode, bodyPreview)
			slog.Warn("HTTP request returned retryable status",
				"status", resp.StatusCode,
				"attempt", attempt+1,
				"max_attempts", c.config.MaxRetries+1,
				"response", bodyPreview,
			)
			continue
		}

		if c.config.LogRequests {
			slog.Debug("HTTP response",
				"status", resp.StatusCode,
				"body_length", len(body),
			)
		}

		return &Response{
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
			Body:       body,
		}, nil
	}

	// All retries exhausted - return the last response body for debugging
	if c.config.ReturnFinalStatus && lastHeaders != nil {
		return &Response{StatusCode: lastStatusCode, Headers: lastHeaders, Body: lastResponseBody}, nil
	}
	if lastErr != nil {
		if len(lastResponseBody) > 0 {
			return &Response{
				StatusCode: lastStatusCode,
				Headers:    lastHeaders,
				Body:       lastResponseBody,
			}, fmt.Errorf("request failed after %d attempts: %w", c.config.MaxRetries+1, lastErr)
		}
		return nil, fmt.Errorf("request failed after %d attempts: %w", c.config.MaxRetries+1, lastErr)
	}
	return nil, fmt.Errorf("request failed after %d attempts with status %d", c.config.MaxRetries+1, lastStatusCode)
}

// RetryAfter reads a Retry-After header (delay in seconds or an HTTP date) as a
// wait: def when absent or unreadable, never more than max.
func RetryAfter(h http.Header, def, max time.Duration) time.Duration {
	d := def
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
			if d < 0 {
				d = 0
			}
		}
	}
	if max > 0 && d > max {
		d = max
	}
	return d
}

// Get performs a GET request
func (c *Client) Get(ctx context.Context, url string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post performs a POST request with the given body
func (c *Client) Post(ctx context.Context, url string, contentType string, body []byte) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// PostJSON performs a POST request with JSON content type
func (c *Client) PostJSON(ctx context.Context, url string, body []byte) (*Response, error) {
	return c.Post(ctx, url, "application/json", body)
}

// calculateBackoff calculates the backoff duration with exponential increase and jitter
func (c *Client) calculateBackoff(attempt int) time.Duration {
	// Exponential backoff: initial * multiplier^attempt
	backoff := float64(c.config.InitialBackoff)
	for i := 0; i < attempt; i++ {
		backoff *= c.config.BackoffMultiplier
	}

	// Cap at max backoff
	if backoff > float64(c.config.MaxBackoff) {
		backoff = float64(c.config.MaxBackoff)
	}

	// Add jitter (± jitterFraction of backoff)
	if c.config.JitterFraction > 0 {
		jitter := backoff * c.config.JitterFraction
		backoff += (rand.Float64()*2 - 1) * jitter
	}

	return time.Duration(backoff)
}

// isRetryableError determines if an error should trigger a retry
func (c *Client) isRetryableError(err error, statusCode int) bool {
	if err != nil {
		errStr := err.Error()
		// Timeout errors
		if strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "deadline exceeded") ||
			strings.Contains(errStr, "TLS handshake timeout") {
			return true
		}
		// Connection errors
		if strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "connection reset") ||
			strings.Contains(errStr, "broken pipe") ||
			strings.Contains(errStr, "EOF") ||
			strings.Contains(errStr, "no such host") {
			return true
		}
		// API-specific retryable errors
		if strings.Contains(errStr, "overloaded") ||
			strings.Contains(errStr, "rate_limit") {
			return true
		}
	}

	return c.isRetryableStatusCode(statusCode)
}

// isRetryableStatusCode checks if a status code should trigger a retry
func (c *Client) isRetryableStatusCode(statusCode int) bool {
	for _, code := range c.config.RetryStatusCodes {
		if statusCode == code {
			return true
		}
	}
	return false
}

// WithTimeout returns a new client with the specified timeout
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	newConfig := c.config
	newConfig.Timeout = timeout
	return New(newConfig)
}
