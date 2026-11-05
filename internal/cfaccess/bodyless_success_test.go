package cfaccess

// Studio answers an admin settings write with 204 No Content, and that response
// carries a Content-Type: text/html it has no use for. The Access detector used
// to look only at the content type on an /api/ path, so it called that success a
// block. On 2026-11-05 `provision apply` therefore wrote the first nav setting,
// reported the Access service token as refused even after a fresh read from
// Secret Manager, and stopped with the rest of the apply undone. The token was
// never the problem.

import (
	"net/http"
	"testing"
)

func TestBodylessSuccessIsNeverABlock(t *testing.T) {
	// The exact shape that caused it: 204, html content type, api path, staging.
	t.Run("204 with a text/html content type", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNoContent)
		})
		resp, err := f.get(t, "https://"+StagingHost+"/api/admin/organizations/18/settings")
		if err != nil {
			t.Fatalf("a 204 is a successful write, not an Access block: %v", err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want the 204 passed through", resp.StatusCode)
		}
	})

	// 205 for the same reason as 204: no body, so a content type says nothing.
	t.Run("205 with a text/html content type", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusResetContent)
		})
		if _, err := f.get(t, "https://"+StagingHost+"/api/x"); err != nil {
			t.Fatalf("205 carries no body either: %v", err)
		}
	})

	// A 200 that really does carry an HTML page stays a block. On the provision
	// path this is the only place it is caught, because studioadmin.Do hands the
	// body back unexamined.
	t.Run("a 200 html page is still a block", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>login</html>"))
		})
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		if AsBlocked(err) == nil {
			t.Fatalf("an HTML page where JSON was expected is still a block, got %v", err)
		}
	})

	// And the detector still catches what it is for.
	t.Run("a non-2xx html page is still a block", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html>Forbidden</html>"))
		})
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		if AsBlocked(err) == nil {
			t.Fatalf("a 403 html page from an Access host is a block, got %v", err)
		}
	})

	t.Run("a redirect to the access login is still a block", func(t *testing.T) {
		f := newFixture(t, accessRedirect)
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		if AsBlocked(err) == nil {
			t.Fatalf("a redirect to the Access login is a block, got %v", err)
		}
	})
}
