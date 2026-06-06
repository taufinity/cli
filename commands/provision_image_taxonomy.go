// provision_image_taxonomy.go — provision support for the per-org image
// classification taxonomy.
//
// PHASE-A STUB. The image_tags lookup table and its accompanying
// taxonomy storage land in Phase B4 of the image-asset POC. The
// taxonomy is referenced by the classify_image playbook step (added in
// Phase B3); the step validates produced tags against the vocabulary
// server-side. Until B4 lands, this handler validates the YAML schema
// and prints the intended action.
//
// Plan reference: cto-as-a-service/docs/plans/2026-05-13-efteling-image-asset-poc.md
package commands

import (
	"fmt"
	"path/filepath"
	"strings"
)

// imageTaxonomyConfig is the YAML shape for an org-scoped image
// classification taxonomy. See studio/image-taxonomy.yaml in the
// customer template.
//
//	version: 1
//	axes:
//	  attraction:
//	    description: Named Efteling attractions
//	    multi: true
//	    terms: [symbolica, baron-1898, ...]
//	  channel:
//	    multi: true
//	    terms: [blog, instagram-square, ...]
//	  ...
//	derived:
//	  aspect_ratio: float
//	  aspect_class: [landscape-16-9, square-1-1, ...]
type imageTaxonomyConfig struct {
	Version int                          `yaml:"version"`
	Axes    map[string]imageTaxonomyAxis `yaml:"axes"`
	// TODO(B4): tighten `Derived` schema. Today this accepts arbitrary
	// shapes (mixed strings and []string) because the spec is still
	// evolving (aspect_ratio: float vs aspect_class: [...]). When the
	// image_tags storage backend lands, this becomes a typed struct.
	Derived map[string]any `yaml:"derived,omitempty"`
}

type imageTaxonomyAxis struct {
	Description string   `yaml:"description,omitempty"`
	Multi       bool     `yaml:"multi,omitempty"`
	Terms       []string `yaml:"terms"`
}

// upsertImageTaxonomy validates the YAML and prints the intended action.
// Real upsert lands in Phase B4 alongside the image_tags table and the
// taxonomy storage decision (per-org JSONB on org_settings vs dedicated
// table — see plan Phase 0 step 4).
func upsertImageTaxonomy(c *provisionClient, orgID uint, cfg imageTaxonomyConfig) error {
	_ = c // reserved for the c.writeForOrg call once Phase B4 lands
	if cfg.Version == 0 {
		return fmt.Errorf("image-taxonomy: version is required (use `version: 1`)")
	}
	if len(cfg.Axes) == 0 {
		return fmt.Errorf("image-taxonomy: at least one axis is required")
	}

	// Total term count for the stub print + sanity (empty axes, dupes,
	// whitespace-only terms). Terms are NOT trimmed in-place: a YAML term
	// with leading/trailing whitespace is treated as malformed input,
	// not silently normalised — same value would go to the server later.
	total := 0
	emptyAxes := []string{}
	for name, axis := range cfg.Axes {
		if len(axis.Terms) == 0 {
			emptyAxes = append(emptyAxes, name)
			continue
		}
		total += len(axis.Terms)
		seen := map[string]bool{}
		for _, term := range axis.Terms {
			if term != strings.TrimSpace(term) || term == "" {
				return fmt.Errorf("image-taxonomy axis %q: term %q is empty or has surrounding whitespace", name, term)
			}
			if seen[term] {
				return fmt.Errorf("image-taxonomy axis %q: duplicate term %q", name, term)
			}
			seen[term] = true
		}
	}
	if len(emptyAxes) > 0 {
		return fmt.Errorf("image-taxonomy: axes have no terms: %v", emptyAxes)
	}

	fmt.Printf("STUB image-taxonomy version=%d org=%d (axes=%d, total_terms=%d) — handler pending Phase B4 (image_tags table + taxonomy persistence)\n",
		cfg.Version, orgID, len(cfg.Axes), total)
	return nil
}

func applyImageTaxonomy(c *provisionClient, dir string, orgID uint) error {
	tf := filepath.Join(dir, "image-taxonomy.yaml")
	if !fileExists(tf) {
		return nil
	}
	var cfg imageTaxonomyConfig
	mustReadYAML(tf, &cfg)
	return upsertImageTaxonomy(c, orgID, cfg)
}
