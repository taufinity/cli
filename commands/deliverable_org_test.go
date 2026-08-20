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
