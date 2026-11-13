package commands

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A Cloudflare firewall block (real page, captured 2026-11-13 from staging)
// is reported as a firewall block with its Ray ID, not as "geen verbinding"
// or as a refused Access token.
func TestBannerPreviewFirewallBlockIsNamed(t *testing.T) {
	page, err := os.ReadFile("../internal/jsonresp/testdata/cloudflare_waf_block.html")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeBanner(t)
	f.status, f.statusType, f.body = 403, "text/html; charset=UTF-8", string(page)
	_, err = f.source(t, templateDir(t, nil)).Fill(context.Background())
	if err == nil {
		t.Fatal("a firewall block filled a banner")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "blocked by the firewall") || !strings.Contains(msg, "Ray ID a5a146b38b78b95a") {
		t.Fatalf("message = %q, want a firewall block with its Ray ID", msg)
	}
	for _, wrong := range []string{"geen verbinding", "refused", "rotated"} {
		if strings.Contains(msg, wrong) {
			t.Fatalf("message %q says %q; the firewall judged the request, not the network or the token", msg, wrong)
		}
	}
}
