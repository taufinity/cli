package cfaccess

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"testing"
)

// The fixture is a real Cloudflare firewall block page, captured on
// 2026-11-13 from a template upload to staging that the managed WAF rules
// refused (client IP replaced by 192.0.2.1).
func wafPage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../jsonresp/testdata/cloudflare_waf_block.html")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A firewall block on staging is not an Access refusal: the transport passes
// the page on whole, sends the request once, and does not re-read the token.
func TestStagingFirewallBlockIsNotAnAccessRefusal(t *testing.T) {
	page := wafPage(t)
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(page)
	})
	resp, err := f.client.Post("https://"+StagingHost+"/api/creatives/banner-preview", "application/zip", bytes.NewReader([]byte("PK\x03\x04 binary")))
	if err != nil {
		t.Fatalf("a firewall page must be passed on for the API client to report, got %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, page) {
		t.Fatalf("body changed by the peek: got %d bytes, want %d", len(got), len(page))
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if f.seen.hits != 1 {
		t.Fatalf("server hits = %d; a firewall block must not be retried with another token", f.seen.hits)
	}
	if n := f.fetches.Load(); n != 2 {
		t.Fatalf("gcloud fetches = %d, want 2 (the first token only, no re-read)", n)
	}
}

// Any other 403 HTML page on a staging API path is still an Access refusal.
func TestStagingOtherForbiddenPageIsStillBlocked(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><title>Forbidden</title><div class="cf-error-details">Access denied</div></html>`))
	})
	_, err := f.get(t, "https://"+StagingHost+"/api/x")
	if AsBlocked(err) == nil {
		t.Fatalf("want *BlockedError for a non-firewall 403 page, got %v", err)
	}
}
