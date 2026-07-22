package commands

import (
	"fmt"
	"os"
	"path/filepath"
)

// pullProvisionPresentationTemplates writes every presentation template
// visible to the org into dir/presentation-templates/, one <slug>.html per
// template, unconditionally (unlike dashboards-pull, which only refreshes
// slugs already tracked locally). Presentation templates are typically few
// per org and the primary use case is bootstrapping a source of truth from
// scratch — an org with zero local files still needs its first pull to
// produce something to edit.
//
// Filenames are slugified from Name for a stable, greppable path, but the
// header's own name: field is what apply actually reads, so a rename in
// Studio surfaces as a content diff on next pull, not a silent orphan file.
func pullProvisionPresentationTemplates(c *provisionClient, orgID uint, dir string, dryRun bool) error {
	existing, err := listPresentationTemplates(c, orgID)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		fmt.Println("provision: no presentation templates found for org")
		return nil
	}

	if !dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	seenPath := map[string]string{} // path → uuid, to catch two templates slugifying to the same filename
	pulled := 0
	for _, d := range existing {
		slug := provisionSlugify(d.Name)
		if slug == "" {
			slug = d.UUID
		}
		path := filepath.Join(dir, slug+".html")
		if prevUUID, dup := seenPath[path]; dup {
			fmt.Printf("  WARN: %q and a previous template (uuid=%s) both slugify to %s — skipping this one\n",
				d.Name, prevUUID, path)
			continue
		}
		seenPath[path] = d.UUID

		if dryRun {
			fmt.Printf("WOULD PULL %s uuid=%s → %s\n", d.Name, d.UUID, path)
			pulled++
			continue
		}

		meta := presentationTemplateMeta{
			Name:      d.Name,
			UUID:      d.UUID,
			IsDefault: d.IsDefault,
			Branch:    d.Branch,
		}
		// Written byte-for-byte as returned by the server (no trailing-newline
		// normalization) — apply's NOOP check compares content verbatim
		// against compiled_template, so any padding added here would make a
		// fresh pull -> apply falsely report an UPDATE.
		if err := os.WriteFile(path, renderPresentationTemplateFile(meta, d.CompiledTemplate), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("PULL   %s uuid=%s → %s\n", d.Name, d.UUID, path)
		pulled++
	}

	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	fmt.Printf("provision: presentation templates pull summary: %s=%d\n", verb, pulled)
	return nil
}
