package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type provisionClient struct {
	base          string
	token         string
	dryRun        bool
	noInviteEmail bool
	http          *http.Client
	warnings      []string
}

func newProvisionClient(base, token string, dryRun bool) *provisionClient {
	return &provisionClient{
		base:   base,
		token:  token,
		dryRun: dryRun,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *provisionClient) Warn(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	c.warnings = append(c.warnings, msg)
	fmt.Printf("  WARN: %s\n", msg)
}

func (c *provisionClient) WarningCount() int { return len(c.warnings) }

func (c *provisionClient) get(path string) ([]byte, int, error) {
	return c.getWithHeaders(path, nil)
}

func (c *provisionClient) getForOrg(path string, orgID uint) ([]byte, int, error) {
	return c.getWithHeaders(path, map[string]string{
		"X-Organization-ID": fmt.Sprintf("%d", orgID),
	})
}

func (c *provisionClient) getWithHeaders(path string, extra map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api"+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Accept", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func (c *provisionClient) post(path string, payload []byte) ([]byte, int, error) {
	return c.write(http.MethodPost, path, payload)
}

func (c *provisionClient) put(path string, payload []byte) ([]byte, int, error) {
	return c.write(http.MethodPut, path, payload)
}

func (c *provisionClient) patch(path string, payload []byte) ([]byte, int, error) {
	return c.write(http.MethodPatch, path, payload)
}

func (c *provisionClient) write(method, path string, payload []byte) ([]byte, int, error) {
	return c.writeWithHeaders(method, path, payload, nil)
}

// writeForOrg posts/puts/patches with an X-Organization-ID header set.
func (c *provisionClient) writeForOrg(method, path string, payload []byte, orgID uint) ([]byte, int, error) {
	return c.writeWithHeaders(method, path, payload, map[string]string{
		"X-Organization-ID": fmt.Sprintf("%d", orgID),
	})
}

// writeWithHeaders injects X-Change-Source: provision on every write so version
// rows are attributable to provision in audit logs. Also sets the custom User-Agent
// required to bypass Cloudflare's WAF rules that block Go's default UA on POST.
func (c *provisionClient) writeWithHeaders(method, path string, payload []byte, extra map[string]string) ([]byte, int, error) {
	if c.dryRun {
		fmt.Printf("[dry-run] %s /api%s  payload=%s\n", method, path, provisionSummarize(payload))
		return []byte(`{}`), 200, nil
	}
	req, err := http.NewRequest(method, c.base+"/api"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Change-Source", "provision")
	req.Header.Set("User-Agent", "taufinity-provision/1.0")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func (c *provisionClient) uploadMultipartForOrg(path, fileField, filename string, data []byte, orgID uint) ([]byte, int, error) {
	if c.dryRun {
		fmt.Printf("[dry-run] POST /api%s  multipart file=%s (%d bytes) org=%d\n", path, filename, len(data), orgID)
		return []byte(`{}`), 200, nil
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		return nil, 0, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, 0, err
	}
	if err := mw.Close(); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/api"+path, &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Organization-ID", fmt.Sprintf("%d", orgID))
	req.Header.Set("X-Change-Source", "provision")
	req.Header.Set("User-Agent", "taufinity-provision/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// resolveProvisionOrgID looks up the numeric org ID for the given slug.
// Named resolveProvision* to avoid collision with resolveOrgID in dashboards.go
// (same package, different signature — Go has no function overloading).
func resolveProvisionOrgID(c *provisionClient, slug string) (uint, error) {
	body, status, err := c.get("/organizations")
	if err != nil || status != 200 {
		return 0, fmt.Errorf("list orgs: status=%d err=%v", status, err)
	}
	var orgs []struct {
		ID   uint   `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &orgs); err != nil {
		var wrapped struct {
			Data []struct {
				ID   uint   `json:"id"`
				Slug string `json:"slug"`
			} `json:"data"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return 0, fmt.Errorf("parse orgs: %w", err)
		}
		for _, o := range wrapped.Data {
			if strings.EqualFold(o.Slug, slug) {
				return o.ID, nil
			}
		}
		return 0, fmt.Errorf("org with slug %q not found", slug)
	}
	for _, o := range orgs {
		if strings.EqualFold(o.Slug, slug) {
			return o.ID, nil
		}
	}
	return 0, fmt.Errorf("org with slug %q not found", slug)
}

func provisionSummarize(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}

func provisionAPIErr(prefix string, status int, body []byte, err error) error {
	if len(body) == 0 {
		return fmt.Errorf("%s: status=%d err=%v", prefix, status, err)
	}
	return fmt.Errorf("%s: status=%d err=%v body=%s", prefix, status, err, provisionSummarize(body))
}
