package commands

import (
	"os"
	"regexp"
)

// pinSlug writes `slug: <newSlug>` into the YAML file at path when the current
// YAML slug is empty or different from newSlug. Preserves all other content and
// formatting (targeted line edit, no YAML round-trip).
//
// If the file already has a top-level `slug: ...` line, it is replaced in place.
// Otherwise the new line is inserted immediately before the `name:` line.
// If neither is found, it is prepended to the file.
//
// The regex anchors to line start (^) with no leading whitespace so that
// `slug:` keys nested inside YAML maps (e.g. inside inputs: blocks) are
// never matched.
func pinSlug(path, currentSlug, newSlug string) error {
	if newSlug == "" || currentSlug == newSlug {
		return nil // nothing to do
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Only match top-level `slug:` (no leading whitespace) to avoid clobbering
	// nested keys like `  slug: something` inside inputs blocks.
	slugLine := regexp.MustCompile(`(?m)^slug:\s+\S.*$`)
	newLine := "slug: " + newSlug

	if slugLine.Match(raw) {
		raw = slugLine.ReplaceAll(raw, []byte(newLine))
	} else {
		// Insert before the top-level `name:` line.
		nameLine := regexp.MustCompile(`(?m)^name:`)
		if loc := nameLine.FindIndex(raw); loc != nil {
			insert := []byte(newLine + "\n")
			raw = append(raw[:loc[0]], append(insert, raw[loc[0]:]...)...)
		} else {
			raw = append([]byte(newLine+"\n"), raw...)
		}
	}

	return os.WriteFile(path, raw, 0644)
}
