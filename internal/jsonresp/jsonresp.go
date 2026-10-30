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
}

func (e *NonJSONError) Error() string {
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
	return &NonJSONError{URL: stripQuery(rawURL), ContentType: mt, Status: status, Title: Title(body)}
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
