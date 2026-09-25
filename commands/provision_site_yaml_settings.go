package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// putSiteSettingsJSON PUTs one settings section of a site from a value read out of site.yaml.
// section is the URL segment: "content" or "ui-translations".
func putSiteSettingsJSON(c *provisionClient, siteID uint, section string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", section, err)
	}
	path := fmt.Sprintf("/sites/%d/settings/%s", siteID, section)
	fmt.Printf("provision: site id %d: PUT settings/%s\n", siteID, section)
	body, status, err := c.put(path, payload)
	if err != nil || status >= 300 {
		return provisionAPIErr("PUT "+path, status, body, err)
	}
	return nil
}

// validateUITranslations refuses a map the renderer would turn into blank chrome. The
// endpoint replaces the whole map, so an empty language or an empty string is not a
// no-op: it removes or blanks text that is live on the site.
func validateUITranslations(t map[string]map[string]string) error {
	if len(t) == 0 {
		return fmt.Errorf("is empty; omit the key instead of sending an empty map, which would delete every translation")
	}
	langs := make([]string, 0, len(t))
	for lang := range t {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		keys := t[lang]
		if strings.TrimSpace(lang) == "" {
			return fmt.Errorf("has a language with an empty code")
		}
		if len(keys) == 0 {
			return fmt.Errorf("language %q has no keys", lang)
		}
		var empty []string
		for k, v := range keys {
			if strings.TrimSpace(v) == "" {
				empty = append(empty, k)
			}
		}
		if len(empty) > 0 {
			sort.Strings(empty)
			return fmt.Errorf("language %q has empty values for %s; the page would render them blank", lang, strings.Join(empty, ", "))
		}
	}
	return nil
}
