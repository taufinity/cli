package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixes from the review of 13 Nov: the page cannot be framed by another site,
// a paid render is never sent twice, and .taufinity/ never reaches git.
func TestPreviewReviewFixes(t *testing.T) {
	t.Run("page_refuses_framing", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServer(t, fs)
		resp, err := http.Get(base + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want DENY", got)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("CSP lacks frame-ancestors 'none': %s", csp)
		}
	})
	t.Run("render_not_retried_on_504", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		fs.renderStatus = http.StatusGatewayTimeout
		fs.renderBody = `{"error":"upstream timeout"}`
		s, base := startRenderServer(t, fs)
		code, _ := postRender(t, base, s.token, "", `{"row":0,"format":"png"}`)
		if code == http.StatusOK {
			t.Fatalf("a 504 render answered 200")
		}
		fs.mu.Lock()
		n := len(fs.renders)
		fs.mu.Unlock()
		if n != 1 {
			t.Fatalf("Studio saw %d render requests for one click, want 1 (a retry can charge twice)", n)
		}
	})
	t.Run("render_client_waits_for_slow_renders", func(t *testing.T) {
		if got := bannerRenderClientTimeout; got < renderTimeout {
			t.Fatalf("render client timeout %v is shorter than the render timeout %v", got, renderTimeout)
		}
	})
	t.Run("key_dir_ignored_by_git", func(t *testing.T) {
		dir := templateDir(t, nil)
		if _, err := templateKeyFor(dir); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, bannerStateDir, ".gitignore"))
		if err != nil {
			t.Fatalf(".taufinity/.gitignore missing: %v", err)
		}
		if strings.TrimSpace(string(raw)) != "*" {
			t.Fatalf(".taufinity/.gitignore = %q, want *", raw)
		}
	})
}
