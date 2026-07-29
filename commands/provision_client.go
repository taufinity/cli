package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/taufinity/cli/pkg/studioadmin"
)

type provisionClient struct {
	base          string
	token         string
	dryRun        bool
	noInviteEmail bool
	http          *http.Client
	warnings      []string

	// workspaceConfigPath points at the analytics workspace config that declares
	// the valid source write keys. It lives with the analytics infrastructure
	// rather than with the site specs, so provision has to be told where it is;
	// empty means "not supplied", and tracker write keys go unvalidated (with a
	// warning). See checkTrackerWriteKey.
	workspaceConfigPath string

	// cfAccessID and cfAccessSecret are a Cloudflare Access service token, sent
	// as CF-Access-Client-Id / CF-Access-Client-Secret on every request. They let
	// provision reach a Studio instance that sits behind Cloudflare Access (a
	// pre-authentication gate in front of the app). Sourced from the environment
	// only — a service token is a credential and must never land in a flag, where
	// it would show up in shell history and process listings, nor in this repo.
	// Empty when either is unset, in which case no CF-Access headers are sent and
	// the request goes straight through (correct for any host not behind Access).
	// These headers only pass Cloudflare's gate; the app's own X-API-Key still
	// authorises every request, so sending them to a host that does not need them
	// is harmless.
	cfAccessID     string
	cfAccessSecret string

	// admin is the shared transport (pkg/studioadmin) — the single code path for
	// JSON get/write, the same client the Terraform provider uses. The local http
	// client above is retained only for the multipart upload path.
	admin *studioadmin.Client
}

func newProvisionClient(base, token string, dryRun bool) *provisionClient {
	// CF-Access service token from the environment. The same variable names
	// the rest of the codebase uses for its Cloudflare Access bypass.
	cfID := os.Getenv("CF_ACCESS_CLIENT_ID")
	cfSecret := os.Getenv("CF_ACCESS_CLIENT_SECRET")
	return &provisionClient{
		base:           base,
		token:          token,
		dryRun:         dryRun,
		http:           &http.Client{Timeout: 30 * time.Second},
		cfAccessID:     cfID,
		cfAccessSecret: cfSecret,
		// CF-Access is applied per-call from cfAccessID/cfAccessSecret (below), not
		// configured on the admin client — some callers (and tests) set the token
		// on the struct after construction, so the struct fields are the source of
		// truth.
		admin: studioadmin.New(base, token, "", studioadmin.WithChangeSource("provision")),
	}
}

// cfAccessInto adds the Cloudflare Access service token to h when BOTH id and secret
// are set (a half-configured pair sends nothing).
func (c *provisionClient) cfAccessInto(h map[string]string) {
	if c.cfAccessID != "" && c.cfAccessSecret != "" {
		h["CF-Access-Client-Id"] = c.cfAccessID
		h["CF-Access-Client-Secret"] = c.cfAccessSecret
	}
}

// setCFAccessHeaders attaches the Cloudflare Access service token to a request,
// but only when both halves are present. Sent on reads and writes alike, because
// Cloudflare Access gates every request — a diff that GETs remote state would 403
// before it ever computed a change if the header only rode the write path.
//
// Never log these values. The dry-run and error paths print method, path and
// status, and must never grow to print headers.
func (c *provisionClient) setCFAccessHeaders(req *http.Request) {
	if c.cfAccessID == "" || c.cfAccessSecret == "" {
		return
	}
	req.Header.Set("CF-Access-Client-Id", c.cfAccessID)
	req.Header.Set("CF-Access-Client-Secret", c.cfAccessSecret)
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
	// Delegate the JSON transport to the shared client (single code path). Accept is
	// GET-specific, so it rides as an extra header; X-API-Key, CF-Access and org are
	// applied by studioadmin.Do.
	h := map[string]string{"Accept": "application/json"}
	c.cfAccessInto(h)
	for k, v := range extra {
		h[k] = v
	}
	return c.admin.Do(context.Background(), http.MethodGet, path, nil, h)
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
// rows are attributable to provision in audit logs. The User-Agent carries the
// binary identity and build (see provisionUserAgent) — it both bypasses
// Cloudflare's WAF rules that block Go's default UA on POST, and makes an audit
// log entry answer "which binary, which build" instead of just "provision".
func (c *provisionClient) writeWithHeaders(method, path string, payload []byte, extra map[string]string) ([]byte, int, error) {
	if c.dryRun {
		fmt.Printf("[dry-run] %s /api%s  payload=%s\n", method, path, provisionSummarize(payload))
		return []byte(`{}`), 200, nil
	}
	// Delegate to the shared client (single code path). X-API-Key, Content-Type and
	// X-Change-Source=provision are applied by studioadmin.Do (change-source was
	// configured on the admin client). User-Agent, CF-Access and any per-call extras
	// (e.g. X-Organization-ID) ride as extra headers.
	h := map[string]string{"User-Agent": provisionUserAgent()}
	c.cfAccessInto(h)
	for k, v := range extra {
		h[k] = v
	}
	return c.admin.Do(context.Background(), method, path, payload, h)
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
	req.Header.Set("User-Agent", provisionUserAgent())
	c.setCFAccessHeaders(req) // uploads are gated by Cloudflare Access too
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("multipart upload: read response body: %w", readErr)
	}
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
