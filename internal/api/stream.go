package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/taufinity/cli/internal/cfaccess"
)

// APIKeyEnv names the environment variable holding an API key. When set, the
// streaming calls send it as X-API-Key instead of the stored login (for CI).
const APIKeyEnv = "TAUFINITY_API_KEY"

// streamClient is an http.Client without a timeout: a streamed review can run
// for minutes, so only the request context bounds it. It never follows a
// redirect, so a request, its credentials and its body can only ever reach the
// URL it was built for.
func streamClient() *http.Client {
	return &http.Client{
		Transport: cfaccess.NewTransport(nil),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// PostStream POSTs a JSON body and returns the response with its body unread,
// for the caller to read as it streams and to close. It is for requests that
// cost money and are not idempotent, so unlike the other calls it never
// retries: not on 429, not on 5xx, not on a network error.
//
// Auth is TAUFINITY_API_KEY as X-API-Key when set, else the stored login. The
// one exception to "no retry" is a 401 on the stored login: the session is
// renewed once and the request sent again, which is safe because a 401 is
// refused before anything runs. Debug logging records the body's size only,
// never its content, and never the key or the token.
func (c *Client) PostStream(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return c.postStream(ctx, path, body, true)
}

// PostStreamOnce is PostStream that never sends the request twice: a 401 is
// returned as it is. The session is still renewed after a 401, so that the
// next run works, but the request itself is not replayed. The review command
// uses it so that Studio sees exactly one POST per run.
func (c *Client) PostStreamOnce(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return c.postStream(ctx, path, body, false)
}

func (c *Client) postStream(ctx context.Context, path string, body []byte, replay bool) (*http.Response, error) {
	if c.dryRun {
		return nil, errors.New("PostStream called in dry-run mode")
	}
	url := c.baseURL + path
	hc := streamClient()

	build := func(set func(*http.Request)) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/x-ndjson")
		c.setOrgHeader(req)
		set(req)
		return req, nil
	}

	if key := os.Getenv(APIKeyEnv); key != "" {
		req, err := build(func(r *http.Request) { r.Header.Set("X-API-Key", key) })
		if err != nil {
			return nil, err
		}
		return c.doStream(hc, req, "X-API-Key", len(body))
	}

	token, err := c.getToken(ctx)
	if err != nil {
		c.logTokenError(err)
		return nil, err
	}
	req, err := build(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	if err != nil {
		return nil, err
	}
	resp, err := c.doStream(hc, req, "Bearer", len(body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.authToken == "" {
		if !replay {
			// Renew for the next run; keep this response, unread.
			if _, rerr := c.RenewAfterReject(ctx, token); rerr != nil {
				c.logTokenError(rerr)
			}
			return resp, nil
		}
		_ = resp.Body.Close()
		fresh, rerr := c.RenewAfterReject(ctx, token)
		if rerr != nil {
			c.logTokenError(rerr)
			return nil, rerr
		}
		req, err = build(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fresh) })
		if err != nil {
			return nil, err
		}
		return c.doStream(hc, req, "Bearer", len(body))
	}
	return resp, nil
}

func (c *Client) doStream(hc *http.Client, req *http.Request, scheme string, bodyLen int) (*http.Response, error) {
	if c.log != nil {
		c.log.Debug("→ request", "method", req.Method, "url", req.URL.String(), "auth", scheme+" (redacted)", "body_bytes", bodyLen)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if c.log != nil {
		c.log.Debug("← response", "status", resp.StatusCode)
	}
	return resp, nil
}

// GetWithKeyOrAuth is a GET with the same credentials as PostStream: the API
// key when TAUFINITY_API_KEY is set, else the stored login.
func (c *Client) GetWithKeyOrAuth(ctx context.Context, path string) (*Response, error) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		return c.GetWithAuth(ctx, path)
	}
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	c.setOrgHeader(req)
	if c.log != nil {
		c.log.Debug("→ request", "method", req.Method, "url", url, "auth", "X-API-Key (redacted)")
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	c.logResponse(resp.StatusCode, nil)
	return &Response{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

// BaseURL is the Studio URL this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }
