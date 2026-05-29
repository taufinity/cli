package telemetry

import (
	"regexp"
)

var (
	// tokenRe matches API keys, JWTs, refresh tokens: alphanumeric + _- at 20+ chars.
	tokenRe = regexp.MustCompile(`[a-zA-Z0-9_\-]{20,}`)
	// emailRe matches email addresses.
	emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	// urlQueryRe matches the query string portion of URLs embedded in messages.
	urlQueryRe = regexp.MustCompile(`(https?://[^\s]*)\?[^\s]*`)
)

// scrub removes token-shaped strings, email addresses, and URL query params from s.
func scrub(s string) string {
	if s == "" {
		return s
	}
	// Strip query params from URLs embedded in the string.
	s = urlQueryRe.ReplaceAllString(s, "$1")
	// Redact emails first (they contain @ which prevents token pattern match).
	s = emailRe.ReplaceAllString(s, "[redacted]")
	// Redact long token-shaped strings.
	s = tokenRe.ReplaceAllString(s, "[redacted]")
	return s
}
