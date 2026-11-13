// Package jsonresp turns an HTML page where the CLI expected JSON into a short,
// plain error, instead of letting a JSON decoder fail on the first '<' or an
// error message dump the whole page.
//
// An HTML answer from a JSON endpoint is almost never the app itself: it is a
// proxy, a load balancer error page, a login wall, or the wrong URL. The error
// names the URL, the content type, the HTTP status and the page <title>, which
// is usually enough to tell which; it never includes the body.
package jsonresp

import (
	"bytes"
	"fmt"
	"html"
	"mime"
	"net/url"
	"regexp"
	"strings"
)

// NonJSONError is an HTML (or otherwise non-JSON) page where JSON was expected.
type NonJSONError struct {
	URL         string // without query string
	ContentType string
	Status      int
	Title       string // the page <title>, if any, trimmed and capped
	// Firewall is set when the page is Cloudflare's firewall block page: the
	// request was stopped at the edge and never reached Studio.
	Firewall bool
	RayID    string // the Cloudflare Ray ID on a firewall page, for support
}

func (e *NonJSONError) Error() string {
	if e.Firewall {
		ray := ""
		if e.RayID != "" {
			ray = ", Ray ID " + e.RayID
		}
		return fmt.Sprintf("blocked by the firewall: Cloudflare stopped the request to %s (HTTP %d%s) before it reached Studio. "+
			"This is not a login or Access problem; the firewall judged the request itself. Report it to Taufinity with the Ray ID", e.URL, e.Status, ray)
	}
	ct := e.ContentType
	if ct == "" {
		ct = "no content type"
	}
	msg := fmt.Sprintf("expected JSON from %s, got %s (HTTP %d)", e.URL, ct, e.Status)
	if e.Title != "" {
		msg += fmt.Sprintf(": page title %q", e.Title)
	}
	return msg
}

// Check returns a *NonJSONError when the response is an HTML page, judged by
// its Content-Type or, when that is missing, by the body starting like one.
// Anything else (JSON, empty, plain text) returns nil and is left to the
// caller.
func Check(rawURL string, status int, contentType string, body []byte) error {
	if !IsHTML(contentType, body) {
		return nil
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt == "" {
		mt = contentType
	}
	e := &NonJSONError{URL: stripQuery(rawURL), ContentType: mt, Status: status, Title: Title(body)}
	if IsFirewallBlock(status, body) {
		e.Firewall, e.RayID = true, RayID(body)
	}
	return e
}

// IsFirewallBlock reports whether body is Cloudflare's firewall (WAF) block
// page, "Sorry, you have been blocked". It is told apart from an Access login
// or denial page, which means a missing or refused token, by the markup only
// the block page has.
func IsFirewallBlock(status int, body []byte) bool {
	if status != 403 {
		return false
	}
	head := firstN(body, 64<<10)
	return bytes.Contains(head, []byte("cf-error-details")) &&
		(bytes.Contains(head, []byte(`data-translate="block_headline"`)) || bytes.Contains(head, []byte("Sorry, you have been blocked")))
}

var rayRE = regexp.MustCompile(`Cloudflare Ray ID:\s*(?:<[^>]*>\s*)*([0-9a-f]{16})`)

// RayID returns the Cloudflare Ray ID printed on a Cloudflare error page.
func RayID(body []byte) string {
	if m := rayRE.FindSubmatch(firstN(body, 64<<10)); m != nil {
		return string(m[1])
	}
	return ""
}

// IsHTML reports whether a response is an HTML page.
func IsHTML(contentType string, body []byte) bool {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		switch mt {
		case "text/html", "application/xhtml+xml":
			return true
		case "application/json":
			return false
		}
	}
	head := bytes.ToLower(bytes.TrimSpace(firstN(body, 512)))
	return bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html"))
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// Title returns the page <title>, whitespace-collapsed and capped at 120 chars.
func Title(body []byte) string {
	m := titleRE.FindSubmatch(firstN(body, 64<<10))
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	if len(t) > 120 {
		t = t[:120] + "..."
	}
	return t
}

func firstN(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func stripQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}
