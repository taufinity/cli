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
// header's own uuid: field is what apply actually matches on. A server-side
// rename changes the slug (new filename) without changing the uuid, so the
// old file would otherwise survive as an orphan — apply's *.html glob still
// picks it up, its uuid still resolves, and its now-stale Name silently
// reverts the rename on next apply. This function removes that orphan once
// the rename's new file has been written.
func pullProvisionPresentationTemplates(c *provisionClient, orgID uint, dir string, dryRun bool) error {
	existing, err := listPresentationTemplates(c, orgID)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		fmt.Println("provision: no presentation templates found for org")
		return nil
	}

	// uuid -> path of whichever pre-existing local file currently carries it,
	// captured before this pull writes anything.
	priorPathByUUID := map[string]string{}
	if prior, err := filepath.Glob(filepath.Join(dir, "*.html")); err == nil {
		for _, p := range prior {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if meta, _ := parsePresentationTemplateFile(raw); meta.UUID != "" {
				priorPathByUUID[meta.UUID] = p
			}
		}
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
		stalePath, renamed := priorPathByUUID[d.UUID]
		renamed = renamed && stalePath != path

		if dryRun {
			fmt.Printf("WOULD PULL %s uuid=%s → %s\n", d.Name, d.UUID, path)
			if renamed {
				fmt.Printf("  WOULD REMOVE stale renamed file %s (superseded by %s)\n", stalePath, path)
			}
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

		if renamed {
			if err := os.Remove(stalePath); err != nil {
				c.Warn("presentation template %q: failed to remove stale renamed file %s: %v", d.Name, stalePath, err)
			} else {
				fmt.Printf("REMOVE %s (renamed to %s)\n", stalePath, path)
			}
		}
	}

	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	fmt.Printf("provision: presentation templates pull summary: %s=%d\n", verb, pulled)
	return nil
}
