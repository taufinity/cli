package commands

import "testing"

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

// TestPortalBaseFor covers the origin used in the link printed after an upload.
// It must be the organization's own portal domain, because the fallback host
// resolves a deliverable against whichever organization the viewer's session is
// currently switched into, and anyone sitting elsewhere gets a bare 404.
func TestPortalBaseFor(t *testing.T) {
	orgs := []portalOrgEntry{
		{ID: 7, PortalDomain: "portal.example.com"},
		{ID: 9},
	}
	const fallback = "https://canonical.example.com"

	if got := portalBaseFor(orgs, "7", fallback); got != "https://portal.example.com" {
		t.Errorf("expected the portal domain, got %q", got)
	}
	// No portal domain configured, and an organization that is not listed at
	// all: both fall back to the canonical host, which still lands on the SPA
	// route rather than the raw API path.
	if got := portalBaseFor(orgs, "9", fallback); got != fallback {
		t.Errorf("expected the fallback host for an org without a portal domain, got %q", got)
	}
	if got := portalBaseFor(orgs, "404", fallback); got != fallback {
		t.Errorf("expected the fallback host for an unknown org, got %q", got)
	}
}
