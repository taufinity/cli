package jsonresp

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// testdata/cloudflare_waf_block.html is a real Cloudflare firewall block page,
// captured on 2026-11-13 from a template upload to staging that the managed
// WAF rules refused (client IP replaced by 192.0.2.1).
func TestFirewallBlockPageGetsItsOwnMessage(t *testing.T) {
	page, err := os.ReadFile("testdata/cloudflare_waf_block.html")
	if err != nil {
		t.Fatal(err)
	}
	err = Check("https://studio.taufinity.io/api/creatives/banner-preview?x=1", 403, "text/html; charset=UTF-8", page)
	var nj *NonJSONError
	if !errors.As(err, &nj) {
		t.Fatalf("want *NonJSONError, got %v", err)
	}
	if !nj.Firewall {
		t.Fatal("the Cloudflare block page was not recognised as a firewall block")
	}
	if nj.RayID != "a5a146b38b78b95a" {
		t.Fatalf("Ray ID = %q, want a5a146b38b78b95a", nj.RayID)
	}
	msg := err.Error()
	for _, want := range []string{
		"blocked by the firewall",
		"https://studio.taufinity.io/api/creatives/banner-preview",
		"HTTP 403, Ray ID a5a146b38b78b95a",
		"not a login or Access problem",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "x=1") || strings.Contains(msg, "192.0.2.1") {
		t.Fatalf("message carries the query string or page content: %q", msg)
	}
}

func TestOtherHTMLIsNotAFirewallBlock(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"access denial page":  {403, `<html><title>Forbidden</title><div class="cf-error-details">Access denied</div></html>`},
		"block page, not 403": {200, `<div class="cf-error-details"><h1 data-translate="block_headline">Sorry, you have been blocked</h1></div>`},
		"bad gateway":         {502, `<html><title>502 Bad Gateway</title></html>`},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check("https://studio.example/api/x", tc.status, "text/html", []byte(tc.body))
			var nj *NonJSONError
			if !errors.As(err, &nj) {
				t.Fatalf("want *NonJSONError, got %v", err)
			}
			if nj.Firewall || strings.Contains(err.Error(), "firewall") {
				t.Fatalf("not a firewall page, but reported as one: %q", err.Error())
			}
		})
	}
}
