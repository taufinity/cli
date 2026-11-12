package review

import (
	"path"
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

var excludedSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".sqlite", ".db", ".dump",
	".sql.gz", ".min.js", ".min.css", ".map",
}

// excludedPath reports whether a repository-relative, slash-separated path
// must never be sent: secrets, local databases, vendored or generated output,
// lock files. node_modules is excluded at any depth; vendor, dist and build
// only at the repository root, where they are output directories rather than
// source folders that happen to share the name.
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
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasPrefix(base, "id_rsa") || lockFiles[base] {
		return true
	}
	lower := strings.ToLower(base)
	for _, s := range excludedSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
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
// content (at least binaryProbe bytes when available).
func skipReason(p string, size int64, head []byte) string {
	switch {
	case excludedPath(p):
		return ReasonExcluded
	case isBinary(head):
		return ReasonBinary
	case size > MaxFileBytes:
		return ReasonTooLarge
	}
	return ""
}
