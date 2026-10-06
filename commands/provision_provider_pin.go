package commands

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
)

type providerItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// pinProviderID writes `id: <liveID>` into the YAML file when the provider was
// just created (cfgID == 0) or when the pinned id in the file is stale. It
// preserves comments and formatting by editing only the top-level id line.
func pinProviderID(path string, cfgID, liveID int) error {
	if liveID == 0 || cfgID == liveID {
		return nil
	}
	if cfgID != 0 {
		fmt.Printf("  WARN: provider id in YAML (%d) differs from server (%d) — updating pin in %s\n", cfgID, liveID, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	idLine := regexp.MustCompile(`(?m)^id:\s+\d+`)
	newIDLine := "id: " + strconv.Itoa(liveID)
	if idLine.Match(raw) {
		raw = idLine.ReplaceAll(raw, []byte(newIDLine))
	} else if loc := regexp.MustCompile(`(?m)^name:`).FindIndex(raw); loc != nil {
		raw = append(raw[:loc[0]], append([]byte(newIDLine+"\n"), raw[loc[0]:]...)...)
	} else {
		raw = append([]byte(newIDLine+"\n"), raw...)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	fmt.Printf("provision: pinned provider id=%d in %s\n", liveID, path)
	return nil
}
