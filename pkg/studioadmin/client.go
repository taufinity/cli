// Package studioadmin is the single code path to the Taufinity Studio admin API.
//
// Both frontends import it: the `taufinity` CLI (provision commands) and the
// Terraform provider. Neither shells out to the other — they share this package,
// exactly as gcloud and the Terraform google provider are both thin clients over
// the GCP API. Auth is the Studio admin token (X-API-Key), the same credential the
// CLI uses.
package studioadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to the Studio admin API. Construct with New.
type Client struct {
	base           string
	token          string
	org            string // organization slug/id for org-scoped admin calls (optional)
	changeSource   string // X-Change-Source header value
	cfAccessID     string // Cloudflare Access service token (optional; both or nothing)
	cfAccessSecret string
	http           *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithChangeSource sets the X-Change-Source header (default "terraform"). The CLI's
// provision frontend sets "provision".
func WithChangeSource(s string) Option { return func(c *Client) { c.changeSource = s } }

// WithCFAccess sets a Cloudflare Access service token, sent only when BOTH id and
// secret are non-empty (a half-configured pair must send nothing).
func WithCFAccess(id, secret string) Option {
	return func(c *Client) { c.cfAccessID, c.cfAccessSecret = id, secret }
}

// New returns a Client for the given base URL (e.g. https://studio.taufinity.io),
// admin token (X-API-Key), and org (sent as X-Organization-ID when numeric, else
// X-Organization-Slug).
func New(base, token, org string, opts ...Option) *Client {
	c := &Client{
		base:         base,
		token:        token,
		org:          org,
		changeSource: "terraform",
		http:         &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIError is a structured admin-API error, so callers (CLI or provider) can surface
// an actionable message rather than a raw HTTP dump.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("studio admin API %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsNotFound reports whether err is a 404 from the admin API — used by callers to
// detect a resource deleted outside Terraform and remove it from state.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Get performs GET /api<path> and unmarshals the JSON body into out (if non-nil).
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, "GET", path, nil, out, nil)
}

// Write performs method /api<path> with a JSON body and unmarshals the response into
// out (if non-nil). extraHeaders is optional (e.g. X-Organization-ID).
func (c *Client) Write(ctx context.Context, method, path string, body any, out any, extraHeaders map[string]string) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("studioadmin: marshal body: %w", err)
		}
		payload = b
	}
	return c.do(ctx, method, path, payload, out, extraHeaders)
}

// Do is the low-level transport shared by both frontends (the provider's typed
// Get/Write below, and the CLI's provision client). It sets the common headers
// (X-API-Key, X-Change-Source, org, CF-Access, Content-Type for bodies), applies
// any extra headers, and returns the raw body + status. It does NOT treat a non-2xx
// as an error — the caller decides (the CLI checks the status int; Get/Write map it
// to an APIError).
func (c *Client) Do(ctx context.Context, method, path string, payload []byte, extra map[string]string) ([]byte, int, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("studioadmin: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("X-Change-Source", c.changeSource)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfAccessID != "" && c.cfAccessSecret != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfAccessID)
		req.Header.Set("CF-Access-Client-Secret", c.cfAccessSecret)
	}
	if c.org != "" {
		// Org-scoped endpoints (e.g. custom-ai-providers) require the numeric
		// X-Organization-ID; others also accept the slug.
		if isNumeric(c.org) {
			req.Header.Set("X-Organization-ID", c.org)
		} else {
			req.Header.Set("X-Organization-Slug", c.org)
		}
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("studioadmin: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("studioadmin: read %s %s response body: %w", method, path, readErr)
	}
	return body, resp.StatusCode, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload []byte, out any, extra map[string]string) error {
	respBody, status, err := c.Do(ctx, method, path, payload, extra)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &APIError{Method: method, Path: path, Status: status, Body: string(respBody)}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("studioadmin: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}
