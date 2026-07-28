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
	"encoding/json"
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

// Get performs GET /api<path> and unmarshals the JSON body into out (if non-nil).
func (c *Client) Get(path string, out any) error {
	return c.do("GET", path, nil, out, nil)
}

// Write performs method /api<path> with a JSON body and unmarshals the response into
// out (if non-nil). extraHeaders is optional (e.g. X-Organization-ID).
func (c *Client) Write(method, path string, body any, out any, extraHeaders map[string]string) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("studioadmin: marshal body: %w", err)
		}
		payload = b
	}
	return c.do(method, path, payload, out, extraHeaders)
}

func (c *Client) do(method, path string, payload []byte, out any, extra map[string]string) error {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.base+"/api"+path, reader)
	if err != nil {
		return fmt.Errorf("studioadmin: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Change-Source", "terraform")
	if c.org != "" {
		req.Header.Set("X-Organization-Slug", c.org)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("studioadmin: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
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
