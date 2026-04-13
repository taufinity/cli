package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	token, err := c.getToken()
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
	token, err := c.getToken()
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
	token, err := c.getToken()
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
	token, err := c.getToken()
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
	token, err := c.getToken()
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

// ValidateAuth checks if authentication is valid, refreshing if needed.
// Call this before starting work to fail fast on auth issues.
func (c *Client) ValidateAuth(ctx context.Context) error {
	creds, err := auth.LoadCredentials()
	if err != nil {
		return err
	}

	// Check local expiry first
	if creds.IsExpired() {
		// Try to refresh
		if err := c.refreshToken(creds); err != nil {
			auth.DeleteCredentials()
			return fmt.Errorf("session expired")
		}
		return nil
	}

	// Validate with server
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/cli/token/validate", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network error - can't validate but don't fail
		return nil
	}

	if resp.StatusCode == http.StatusOK {
		creds.UpdateValidatedAt()
		return nil
	}

	// Token invalid, try refresh
	if err := c.refreshToken(creds); err != nil {
		auth.DeleteCredentials()
		return fmt.Errorf("session invalid")
	}
	return nil
}

// getToken loads and validates the access token.
// If the token needs server-side validation, it validates and refreshes if needed.
func (c *Client) getToken() (string, error) {
	// Use explicit token if set (no validation needed)
	if c.authToken != "" {
		return c.authToken, nil
	}

	// Load from saved credentials
	creds, err := auth.LoadCredentials()
	if err != nil {
		return "", err
	}

	// Check local expiry first
	if creds.IsExpired() {
		// Try to refresh
		if err := c.refreshToken(creds); err != nil {
			// Refresh failed, revoke and return error
			auth.DeleteCredentials()
			return "", fmt.Errorf("session expired, please run 'taufinity auth login'")
		}
		return creds.AccessToken, nil
	}

	// Check if we need server-side validation
	if creds.NeedsValidation() {
		if err := c.validateAndRefreshToken(creds); err != nil {
			// Validation failed and refresh failed, revoke
			auth.DeleteCredentials()
			return "", fmt.Errorf("session invalid, please run 'taufinity auth login'")
		}
	}

	return creds.AccessToken, nil
}

// validateAndRefreshToken validates the token with the server and refreshes if invalid.
func (c *Client) validateAndRefreshToken(creds *auth.Credentials) error {
	ctx := context.Background()

	// Try to validate
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/cli/token/validate", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network error - don't revoke, just skip validation
		return nil
	}

	if resp.StatusCode == http.StatusOK {
		// Token is valid, update last validated time
		creds.UpdateValidatedAt()
		return nil
	}

	// Token is invalid (401), try to refresh
	return c.refreshToken(creds)
}

// refreshToken attempts to refresh the token using the refresh endpoint.
func (c *Client) refreshToken(creds *auth.Credentials) error {
	ctx := context.Background()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/cli/token/refresh", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh failed: %d", resp.StatusCode)
	}

	// Parse refresh response
	var refreshResp struct {
		AccessToken      string     `json:"access_token"`
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

	// Update credentials
	expiresAt := time.Now().Add(30 * 24 * time.Hour) // Default 30 days
	if refreshResp.ExpiresAt != nil {
		expiresAt = *refreshResp.ExpiresAt
	}
	return creds.Update(refreshResp.AccessToken, expiresAt, refreshResp.Email, refreshResp.OrganizationName)
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
