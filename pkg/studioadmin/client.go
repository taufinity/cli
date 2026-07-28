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
	base  string
	token string
	org   string // organization slug for org-scoped admin calls (optional)
	http  *http.Client
}

// New returns a Client for the given base URL (e.g. https://studio.taufinity.io),
// admin token (X-API-Key), and org slug (sent as X-Organization-Slug when non-empty).
func New(base, token, org string) *Client {
	return &Client{
		base:  base,
		token: token,
		org:   org,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
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

func (c *Client) do(ctx context.Context, method, path string, payload []byte, out any, extra map[string]string) error {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, reader)
	if err != nil {
		return fmt.Errorf("studioadmin: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Change-Source", "terraform")
	if c.org != "" {
		// Org-scoped endpoints (e.g. custom-ai-providers) require the numeric
		// X-Organization-ID; others also accept the slug. Send the id form when the
		// org looks numeric, the slug form otherwise.
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
		return fmt.Errorf("studioadmin: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("studioadmin: read %s %s response body: %w", method, path, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(respBody)}
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("studioadmin: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}
