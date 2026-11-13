package review

import (
	"path"
	"regexp"
	"strings"
)

const (
	// MaxFileBytes is the per-file limit: larger files are skipped.
	MaxFileBytes = 1 << 20
	// MaxDiffBytes is the limit for the whole diff after exclusions.
	MaxDiffBytes = 1572864
	// MaxFilesBytes bounds the file contents sent along: Studio's request limit
	// is 8 MiB and the diff may take up to MaxDiffBytes of it.
	MaxFilesBytes = 8<<20 - MaxDiffBytes
	// binaryProbe is how much of a file is searched for a NUL byte.
	binaryProbe = 8 << 10
)

// Skip reasons recorded in Skipped.Reason.
const (
	ReasonExcluded = "excluded"
	ReasonBinary   = "binary"
	ReasonTooLarge = "too_large"
)

var lockFiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"go.sum": true, "Cargo.lock": true, "composer.lock": true,
}

// Credential files by exact name, compared lowercased. One entry per line:
// TestNeverSentPatterns has one case per entry and fails when it is removed.
// (.env itself is caught by the ".env" suffix below.)
var excludedNames = map[string]bool{
	".envrc":           true, // direnv
	".netrc":           true,
	"_netrc":           true, // .netrc on Windows
	".pgpass":          true,
	".npmrc":           true,
	".pypirc":          true,
	".git-credentials": true,
	".dockercfg":       true,
	"credentials.json": true,
}

// Credential files by name prefix, compared lowercased: env variants and SSH
// private keys (the public half is excluded with them).
var excludedPrefixes = []string{
	".env.",
	".env-",
	"id_rsa",
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
}

// Secrets, databases and generated output by name suffix, compared lowercased.
var excludedSuffixes = []string{
	".env",
	".pem",
	".key",
	".p12",
	".pfx",
	".jks",
	".keystore",
	".ppk", // PuTTY private key
	".tfvars",
	".tfvars.json",
	".tfstate",
	".tfstate.backup",
	".sqlite",
	".db",
	".dump",
	".sql.gz",
	".min.js",
	".min.css",
	".map",
}

// excludedPath reports whether a repository-relative, slash-separated path
// must never be read or sent: secrets and credentials, local databases,
// vendored or generated output, lock files. node_modules is excluded at any
// depth; vendor, dist and build only at the repository root, where they are
// output directories rather than source folders that happen to share the name.
func excludedPath(p string) bool {
	p = strings.TrimPrefix(path.Clean(p), "./")
	parts := strings.Split(p, "/")
	for i, seg := range parts[:len(parts)-1] {
		switch seg {
		case "node_modules":
			return true
		case "vendor", "dist", "build":
			if i == 0 {
				return true
			}
		}
	}
	base := parts[len(parts)-1]
	lower := strings.ToLower(base)
	if excludedNames[lower] || lockFiles[base] {
		return true
	}
	// Docker's registry logins: config.json is only a credential file there.
	if lower == "config.json" && len(parts) > 1 && parts[len(parts)-2] == ".docker" {
		return true
	}
	for _, s := range excludedPrefixes {
		if strings.HasPrefix(lower, s) {
			return true
		}
	}
	for _, s := range excludedSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

var (
	serviceAccountType = regexp.MustCompile(`"type"\s*:\s*"service_account"`)
	privateKeyField    = regexp.MustCompile(`"private_key"\s*:`)
)

// serviceAccountKey reports a Google service-account key: a JSON file with
// type service_account and a private_key. These have no fixed name (the
// console names them after the project), so they are recognised by content.
func serviceAccountKey(p string, content []byte) bool {
	return strings.HasSuffix(strings.ToLower(p), ".json") &&
		serviceAccountType.Match(content) && privateKeyField.Match(content)
}

// isBinary reports a NUL byte within the first 8 KiB.
func isBinary(content []byte) bool {
	if len(content) > binaryProbe {
		content = content[:binaryProbe]
	}
	for _, b := range content {
		if b == 0 {
			return true
		}
	}
	return false
}

// skipReason classifies a file before it is read in full: by name first, then
// by size, then by content. size < 0 means unknown. head is the start of the
// content (at least binaryProbe bytes when available). Callers check
// excludedPath before reading, so an excluded name is never read at all.
func skipReason(p string, size int64, head []byte) string {
	switch {
	case excludedPath(p), serviceAccountKey(p, head):
		return ReasonExcluded
	case isBinary(head):
		return ReasonBinary
	case size > MaxFileBytes:
		return ReasonTooLarge
	}
	return ""
}
