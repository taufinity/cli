package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/httpclient"
)

// ErrRefreshTokenRejected is returned by refreshToken when the server
// definitively rejects the refresh token (HTTP 401) — it is invalid, expired,
// or revoked. This is the ONLY condition under which getToken deletes the
// stored credentials and forces a re-login. Transient failures (network
// errors, 5xx) return a different (wrapped) error so credentials survive and
// the next invocation can retry.
var ErrRefreshTokenRejected = errors.New("refresh token rejected; please run 'taufinity auth login'")

// Client is the Taufinity API client with retry/backoff and auth support.
type Client struct {
	baseURL    string
	httpClient *httpclient.Client
	authToken  string // Explicit auth token (overrides saved credentials)
	orgID      string // Organization ID header (X-Organization-ID)
	dryRun     bool
	dryRunOut  io.Writer
	log        *slog.Logger // nil = no debug logging
}

// SetOrg sets the organization ID to include in requests.
func (c *Client) SetOrg(orgID string) {
	c.orgID = orgID
}

// setOrgHeader adds X-Organization-ID header if orgID is set.
func (c *Client) setOrgHeader(req *http.Request) {
	if c.orgID != "" {
		req.Header.Set("X-Organization-ID", c.orgID)
	}
}

// New creates a new API client for the given base URL.
func New(baseURL string) *Client {
	cfg := httpclient.DefaultConfig()
	// CLI-friendly timeouts
	cfg.MaxRetries = 3
	cfg.LogRequests = false

	c := &Client{
		baseURL:    baseURL,
		httpClient: httpclient.New(cfg),
		dryRunOut:  os.Stdout,
	}
	if os.Getenv("TAUFINITY_DEBUG") == "1" {
		c.SetDebug(true)
	}
	return c
}

// SetDebug enables or disables debug request logging via a structured slog logger.
// When enabled, all HTTP requests and non-2xx responses are logged to stderr at Debug level.
func (c *Client) SetDebug(enabled bool) {
	if enabled {
		c.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		c.log = nil
	}
}

// logRequest logs a sanitized summary of an outgoing request.
func (c *Client) logRequest(method, url, token string, body []byte) {
	if c.log == nil {
		return
	}
	authSummary := "(none)"
	if token != "" {
		if len(token) > 12 {
			authSummary = "Bearer " + token[:12] + "..."
		} else {
			authSummary = "Bearer " + token
		}
	}
	args := []any{"method", method, "url", url, "auth", authSummary}
	if len(body) > 0 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		args = append(args, "body", snippet)
	}
	c.log.Debug("→ request", args...)
}

// logResponse logs a summary of an HTTP response.
func (c *Client) logResponse(statusCode int, body []byte) {
	if c.log == nil {
		return
	}
	if statusCode >= 400 {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		c.log.Debug("← response", "status", statusCode, "body", snippet)
	} else {
		c.log.Debug("← response", "status", statusCode)
	}
}

// logTokenError logs a token loading failure.
func (c *Client) logTokenError(err error) {
	if c.log != nil {
		c.log.Debug("getToken failed", "error", err)
	}
}

// SetDryRun enables or disables dry-run mode.
// In dry-run mode, non-GET requests are logged but not executed.
func (c *Client) SetDryRun(enabled bool) {
	c.dryRun = enabled
}

// SetDryRunOutput sets the writer for dry-run output.
func (c *Client) SetDryRunOutput(w io.Writer) {
	c.dryRunOut = w
}

// SetAuth sets an explicit auth token (overrides saved credentials).
func (c *Client) SetAuth(token string) {
	c.authToken = token
}

// Response wraps the HTTP response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Get performs a GET request (always executed, even in dry-run).
func (c *Client) Get(ctx context.Context, path string) (*Response, error) {
	url := c.baseURL + path
	resp, err := c.httpClient.Get(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// GetWithAuth performs an authenticated GET request.
func (c *Client) GetWithAuth(ctx context.Context, path string) (*Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		c.logTokenError(err)
		return nil, err
	}

	url := c.baseURL + path
	c.logRequest("GET", url, token, nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c.setOrgHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	c.logResponse(resp.StatusCode, resp.Body)
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// DeleteWithAuth performs an authenticated DELETE request.
func (c *Client) DeleteWithAuth(ctx context.Context, path string) (*Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		c.logTokenError(err)
		return nil, err
	}

	if c.dryRun {
		c.printDryRun("DELETE", path, "", nil)
		return &Response{StatusCode: http.StatusOK}, nil
	}

	url := c.baseURL + path
	c.logRequest("DELETE", url, token, nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c.setOrgHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	c.logResponse(resp.StatusCode, resp.Body)
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// PostJSON performs a POST request with JSON body.
func (c *Client) PostJSON(ctx context.Context, path string, body any) (*Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	if c.dryRun {
		c.printDryRun("POST", path, "application/json", jsonBody)
		return &Response{StatusCode: http.StatusOK}, nil
	}

	url := c.baseURL + path
	resp, err := c.httpClient.PostJSON(ctx, url, jsonBody)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// PostJSONWithAuth performs an authenticated POST request with JSON body.
func (c *Client) PostJSONWithAuth(ctx context.Context, path string, body any) (*Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		c.logTokenError(err)
		return nil, err
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	if c.dryRun {
		c.printDryRun("POST", path, "application/json", jsonBody)
		return &Response{StatusCode: http.StatusOK}, nil
	}

	url := c.baseURL + path
	c.logRequest("POST", url, token, jsonBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	c.setOrgHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	c.logResponse(resp.StatusCode, resp.Body)
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// UploadFile performs a multipart file upload.
func (c *Client) UploadFile(ctx context.Context, path, filePath string) (*Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	// Get file info for dry-run
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}

	if c.dryRun {
		c.printDryRunUpload("POST", path, filePath, fileInfo.Size())
		return &Response{StatusCode: http.StatusOK}, nil
	}

	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		return nil, fmt.Errorf("copy file: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close writer: %w", err)
	}

	// Create request
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	c.setOrgHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// PostMultipart performs an authenticated POST request with multipart form data.
func (c *Client) PostMultipart(ctx context.Context, path string, body io.Reader, contentType string) (*Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if c.dryRun {
		fmt.Fprintf(c.dryRunOut, "[dry-run] POST %s\n", path)
		fmt.Fprintf(c.dryRunOut, "  Content-Type: %s\n", contentType)
		return &Response{StatusCode: http.StatusOK}, nil
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	c.setOrgHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	}, nil
}

// ValidateAuth checks that a usable access token can be produced, renewing it
// from the refresh token if it is expired or near expiry. Call this before
// starting work to fail fast on auth issues.
//
// It is a thin wrapper over the single renewing token path (getToken/Token):
// renewal — and the delete-on-definitive-401 behavior — lives in exactly one
// place. A still-valid (not near-expiry) token passes without a server round
// trip; a genuinely revoked token surfaces as a 401 on the first real request.
func (c *Client) ValidateAuth(ctx context.Context) error {
	if _, err := c.Token(ctx); err != nil {
		return err
	}
	return nil
}

// Token returns a usable access token, renewing it from the refresh token when
// it is expired or near expiry. This is the single public entry point for any
// "give me a usable access token" need (auth token, MCP config writers, the
// stdio bridge): it routes through the same renewing path as the authenticated
// request helpers, so every caller gets fresh, rotated tokens.
//
// The ctx bounds the renewal HTTP call (including retry backoff), so a caller
// can cancel a slow refresh.
func (c *Client) Token(ctx context.Context) (string, error) {
	return c.getToken(ctx)
}

// getToken loads the access token, renewing it from the refresh token when it
// is expired or near expiry. The access token is short-lived (1h), so renewal
// happens proactively via ShouldRenew rather than waiting for a 401. The ctx
// bounds the renewal request.
func (c *Client) getToken(ctx context.Context) (string, error) {
	// Use explicit token if set (no renewal needed).
	if c.authToken != "" {
		return c.authToken, nil
	}

	// Load from saved credentials.
	creds, err := auth.LoadCredentials()
	if err != nil {
		return "", err
	}

	// Renew proactively when the short-lived access token is expired or near expiry.
	if creds.ShouldRenew() {
		if err := c.refreshToken(ctx, creds); err != nil {
			// Only a DEFINITIVE rejection (401: refresh token invalid/expired/
			// revoked) clears credentials and forces a re-login. Transient
			// failures (network error, 5xx) leave credentials in place so the
			// next invocation can retry — a flaky server must not log the user
			// out.
			if errors.Is(err, ErrRefreshTokenRejected) {
				auth.DeleteCredentials()
				return "", fmt.Errorf("session expired, please run 'taufinity auth login'")
			}
			return "", fmt.Errorf("token renewal failed (credentials preserved): %w", err)
		}
	}

	return creds.AccessToken, nil
}

// refreshToken exchanges the stored refresh token for a new access+refresh pair.
// It POSTs {refresh_token} (the refresh token, NOT the access token) so it works
// even after the short-lived access token has expired, and stores the rotated
// pair returned by the server.
func (c *Client) refreshToken(ctx context.Context, creds *auth.Credentials) error {
	if !creds.HasRefreshToken() {
		return fmt.Errorf("no refresh token; please run 'taufinity auth login'")
	}

	body, err := json.Marshal(map[string]string{"refresh_token": creds.RefreshToken})
	if err != nil {
		return fmt.Errorf("marshal refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/cli/token/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// httpClient.Do returns *httpclient.Response (body already read into Body
	// []byte). On a non-retryable rejection (e.g. 401) it returns the response
	// with a nil error; on exhausted retries (5xx) it may return both a
	// response AND an error; on a transport/network failure it returns a nil
	// response and an error. Inspect the status FIRST so a 401 is classified as
	// a definitive rejection regardless of how the transport surfaced it.
	resp, err := c.httpClient.Do(req)
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		// Definitive: the refresh token is invalid/expired/revoked. The caller
		// (getToken) treats this — and only this — as grounds to delete creds.
		return ErrRefreshTokenRejected
	}
	if err != nil {
		// Network error or exhausted retries on a transient status (5xx) — the
		// refresh token may still be perfectly valid. Surface a generic error
		// so credentials are preserved.
		return fmt.Errorf("refresh request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Any other non-200 (e.g. 400, 403). Treat as transient/unexpected and
		// preserve credentials rather than nuking the session on a server quirk.
		return fmt.Errorf("refresh failed: %d", resp.StatusCode)
	}

	var refreshResp struct {
		AccessToken      string     `json:"access_token"`
		RefreshToken     string     `json:"refresh_token"`
		ExpiresAt        *time.Time `json:"expires_at"`
		Email            string     `json:"email"`
		OrganizationName string     `json:"organization_name"`
	}
	if err := json.Unmarshal(resp.Body, &refreshResp); err != nil {
		return fmt.Errorf("parse refresh response: %w", err)
	}
	if refreshResp.AccessToken == "" {
		return fmt.Errorf("no access token in refresh response")
	}

	// Default expiry mirrors the server's 1h CLI access token if absent.
	expiresAt := time.Now().Add(time.Hour)
	if refreshResp.ExpiresAt != nil {
		expiresAt = *refreshResp.ExpiresAt
	}
	return creds.UpdateTokens(refreshResp.AccessToken, refreshResp.RefreshToken, expiresAt, refreshResp.Email, refreshResp.OrganizationName)
}

// RevokeRefreshToken revokes a single CLI refresh token server-side (logout).
// Unauthenticated: possession of the refresh token is the authorization. The
// refresh token travels in the body, so this works even when the access token
// has already expired. Best-effort — callers may ignore network errors.
func (c *Client) RevokeRefreshToken(refreshToken string) error {
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return fmt.Errorf("marshal revoke request: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.baseURL+"/api/cli/token/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := c.httpClient.Do(req); err != nil {
		return err
	}
	return nil
}

// RevokeAllRefreshTokens revokes ALL of the authenticated user's CLI sessions
// ("log out everywhere"). Uses normal auth — getToken() auto-refreshes the
// access token first if it has expired, so this works after a long gap too.
func (c *Client) RevokeAllRefreshTokens() error {
	token, err := c.getToken(context.Background())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.baseURL+"/api/cli/token/revoke-all", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke-all failed: %d", resp.StatusCode)
	}
	return nil
}

// printDryRun outputs a dry-run message for a request.
func (c *Client) printDryRun(method, path, contentType string, body []byte) {
	fmt.Fprintf(c.dryRunOut, "[dry-run] %s %s\n", method, path)
	fmt.Fprintf(c.dryRunOut, "  Content-Type: %s\n", contentType)
	if len(body) > 0 {
		// Pretty print JSON if possible
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, body, "  ", "  "); err == nil {
			fmt.Fprintf(c.dryRunOut, "  Body:\n  %s\n", prettyJSON.String())
		} else {
			fmt.Fprintf(c.dryRunOut, "  Body: %s\n", string(body))
		}
	}
}

// printDryRunUpload outputs a dry-run message for a file upload.
func (c *Client) printDryRunUpload(method, path, filePath string, size int64) {
	fmt.Fprintf(c.dryRunOut, "[dry-run] %s %s\n", method, path)
	fmt.Fprintf(c.dryRunOut, "  Content-Type: multipart/form-data\n")
	fmt.Fprintf(c.dryRunOut, "  File: %s (%d bytes)\n", filepath.Base(filePath), size)
}

// DecodeJSON decodes the response body as JSON.
func (r *Response) DecodeJSON(v any) error {
	return json.Unmarshal(r.Body, v)
}

// IsSuccess returns true if the status code is 2xx.
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// Error returns an error if the response is not successful.
func (r *Response) Error() error {
	if r.IsSuccess() {
		return nil
	}
	return fmt.Errorf("API error: %d - %s", r.StatusCode, string(r.Body))
}
