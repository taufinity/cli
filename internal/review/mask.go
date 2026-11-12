package review

import (
	"regexp"
	"strings"
)

const maskedValue = "[MASKED]"

// secretPatterns replace the whole match with [MASKED]. The PEM pattern runs
// first and spans lines (the "+" of a diff included).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`),
	// A block cut short by the diff context: the header and the base64 body.
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----(?:\s*[+ -]?[A-Za-z0-9+/=]{16,})+`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`[sr]k_live_[A-Za-z0-9]{10,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// assignment: a name containing password, passwd, secret, token, api_key or
// apikey, then = or : or :=, then a quoted value of 8 or more characters. The
// name may itself be quoted (JSON, YAML flow style).
var assignment = regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(?:password|passwd|secret|token|api_key|apikey)[A-Za-z0-9_.\-]*["']?\s*(?::=|=|:)\s*)(["'])([^"'\r\n]{8,})(["'])`)

// Mask replaces secret values in s with [MASKED] and returns the result and
// the number of replacements. It never returns or logs the values.
func Mask(s string) (string, int) {
	n := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(string) string {
			n++
			return maskedValue
		})
	}
	s = assignment.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignment.FindStringSubmatch(m)
		if sub[3] == maskedValue || strings.Contains(sub[3], maskedValue) {
			return m
		}
		n++
		return sub[1] + sub[2] + maskedValue + sub[4]
	})
	return s, n
}
