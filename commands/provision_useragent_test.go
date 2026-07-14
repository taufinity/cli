package commands

import (
	"strings"
	"testing"
)

// Two separate provision implementations sent the same hardcoded product token, so
// an audit log could not say which binary performed a write, let alone which build.
// These tests pin the properties that make a write attributable.
func TestProvisionUserAgent_IdentifiesTheBinary(t *testing.T) {
	ua := provisionUserAgent()

	if !strings.HasPrefix(ua, "taufinity-cli/") {
		t.Fatalf("user agent must name the binary, got %q", ua)
	}
	// The old shared literal must not come back. If it does, the two binaries are
	// indistinguishable again and forensics is back to guessing.
	if strings.Contains(ua, "taufinity-provision/1.0") {
		t.Fatalf("user agent reuses the ambiguous shared literal: %q", ua)
	}
}

func TestProvisionUserAgent_CarriesTheBuild(t *testing.T) {
	ua := provisionUserAgent()

	if !strings.Contains(ua, "provision") {
		t.Errorf("user agent should say which subsystem is writing, got %q", ua)
	}
	if !strings.Contains(ua, "commit=") {
		t.Fatalf("user agent must carry the build commit — a version alone cannot tell you "+
			"which code applied a destructive change, got %q", ua)
	}
}

// Stable enough to filter on in an audit query or a log-based alert.
func TestProvisionUserAgent_IsParseable(t *testing.T) {
	ua := provisionUserAgent()

	product, detail, ok := strings.Cut(ua, " (")
	if !ok || !strings.HasSuffix(detail, ")") {
		t.Fatalf("want `product/version (detail)`, got %q", ua)
	}
	if _, version, ok := strings.Cut(product, "/"); !ok || version == "" {
		t.Fatalf("product token must carry a version, got %q", product)
	}
}

// Computed once, but must not vary between calls — an audit trail that changes
// identity mid-run is worse than one that is merely vague.
func TestProvisionUserAgent_IsStable(t *testing.T) {
	if a, b := provisionUserAgent(), provisionUserAgent(); a != b {
		t.Fatalf("user agent is not stable: %q vs %q", a, b)
	}
}
