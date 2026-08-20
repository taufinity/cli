package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/taufinity/cli/internal/api"
)

// TestDeliverableClientUsesLocalOrgFlag pins the flag-shadowing fix.
//
// `deliverable upload` declares its own --org, which shadows the persistent
// flag of the same name, so GetOrg() returns empty even when the user typed
// --org. Before the fix the organization was written into the multipart body
// only, leaving the request itself scoped to whatever organization the session
// was last switched into, so the server authorized against a different
// organization than the one named on the command line.
func TestDeliverableClientUsesLocalOrgFlag(t *testing.T) {
	prev := deliverableOrg
	t.Cleanup(func() { deliverableOrg = prev })

	deliverableOrg = "4242"
	if got := newDeliverableClient().Org(); got != "4242" {
		t.Fatalf("expected the client to be scoped to 4242 from --org, got %q", got)
	}

	deliverableOrg = ""
	if got := newDeliverableClient().Org(); got != GetOrg() {
		t.Fatalf("without --org the client should fall back to the persistent flag %q, got %q", GetOrg(), got)
	}
}

// TestDeliverableURLUsesPortalDomain covers the link printed after an upload.
// It must be the portal SPA route on the organization's own domain: the
// /api/deliverables path serves the file directly and resolves against the
// viewer's currently selected organization, so anyone whose session sits in a
// different organization sees a bare 404.
func TestDeliverableURLUsesPortalDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/organizations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"id":7,"portal_domain":"portal.example.com"},{"id":9}]`))
	}))
	defer srv.Close()

	client := api.New(srv.URL)

	if got := deliverableURL(client, "7", "abc"); got != "https://portal.example.com/deliverables/abc" {
		t.Errorf("expected the portal domain link, got %q", got)
	}

	// Organization 9 has no portal domain, so the canonical host is used. That
	// still lands on the SPA route rather than the raw API path.
	want := strings.TrimSuffix(GetAPIURL(), "/") + "/deliverables/abc"
	if got := deliverableURL(client, "9", "abc"); got != want {
		t.Errorf("expected the fallback link %q, got %q", want, got)
	}
}
