package commands

import (
	"fmt"
	"regexp"
	"strings"
)

// uploadWidgetSlugPattern matches `"ui:uploadWidgetSlug": "some-slug"` in a
// JSON string. Captured group 1 is the slug. Tolerates spacing around the
// colon.
var uploadWidgetSlugPattern = regexp.MustCompile(
	`"ui:uploadWidgetSlug"\s*:\s*"([^"]+)"`,
)

// substituteWidgetSlugs scans a playbook's agent_input_schema (a JSON string)
// for `"ui:uploadWidgetSlug": "<slug>"` directives, resolves each slug to the
// widget's numeric id in the target org, and rewrites the match as
// `"ui:uploadWidgetId": <id>`. This keeps the yaml portable across
// environments where widget ids differ.
//
// Returns the original string verbatim when no matches are found, so the
// hot-path overhead for playbooks without upload directives is one regex
// search and zero allocations.
func substituteWidgetSlugs(c *provisionClient, orgID uint, schemaJSON string) (string, error) {
	matches := uploadWidgetSlugPattern.FindAllStringSubmatchIndex(schemaJSON, -1)
	if len(matches) == 0 {
		return schemaJSON, nil
	}

	// Collect unique slugs.
	slugs := make(map[string]bool)
	for _, m := range matches {
		slug := schemaJSON[m[2]:m[3]]
		slugs[slug] = true
	}

	// Fetch widget list once and build slug→id map.
	body, status, err := c.getForOrg("/widgets/", orgID)
	if err != nil || status != 200 {
		return "", fmt.Errorf("list widgets for slug resolution: status=%d err=%v", status, err)
	}
	var widgets []widgetListItem
	if err := unmarshalListEnvelope(body, &widgets); err != nil {
		return "", fmt.Errorf("parse widgets for slug resolution: %w", err)
	}
	bySlug := make(map[string]uint, len(widgets))
	byName := make(map[string]uint, len(widgets))
	for _, w := range widgets {
		if w.Slug != "" {
			bySlug[w.Slug] = w.ID
		}
		byName[strings.ToLower(w.Name)] = w.ID
	}

	// Resolve every referenced slug. Fall back to name match for the
	// transition case where a widget hasn't been updated to carry the slug yet.
	resolved := make(map[string]uint, len(slugs))
	var missing []string
	for slug := range slugs {
		if id, ok := bySlug[slug]; ok {
			resolved[slug] = id
			continue
		}
		// Fallback: slug looks like "wvs-hoveniers-intake" → try names like
		// "WVS Hoveniers Intake" (slug = lowercase with " " → "-").
		nameGuess := strings.ReplaceAll(slug, "-", " ")
		if id, ok := byName[nameGuess]; ok {
			fmt.Printf("  WARN: widget slug %q resolved via name fallback to id=%d — "+
				"add slug: %s to widget.yaml so future runs are stable\n", slug, id, slug)
			resolved[slug] = id
			continue
		}
		missing = append(missing, slug)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown widget slug(s) referenced in agent_input_schema: %s — "+
			"either the widget isn't provisioned yet (apply widget.yaml first) or the slug is a typo",
			strings.Join(missing, ", "))
	}

	// Walk matches back-to-front so earlier-index replacements don't shift
	// later match offsets.
	out := schemaJSON
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		slug := out[m[2]:m[3]]
		id := resolved[slug]
		replacement := fmt.Sprintf(`"ui:uploadWidgetId": %d`, id)
		out = out[:m[0]] + replacement + out[m[1]:]
	}
	return out, nil
}
