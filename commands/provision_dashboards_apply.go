package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// applyDashboards reads all *.json files from dir/dashboards/ and upserts them
// via the admin dashboard-definitions API. Returns the number of dashboards that
// diffed (drift count), which the caller uses for --strict mode exit codes.
//
// Reuses apiCall, fetchExistingDefs, createDefinition, and updateDefinition from
// dashboards.go. Dry-run is controlled by c.dryRun.
func applyDashboards(c *provisionClient, dir string, orgID, providerID uint, draft bool, previewDataset string) (int, error) {
	dashDir := filepath.Join(dir, "dashboards")
	if !fileExists(dashDir) {
		return 0, nil
	}

	entries, err := filepath.Glob(filepath.Join(dashDir, "*.json"))
	if err != nil {
		return 0, fmt.Errorf("glob dashboards/: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	sort.Strings(entries)

	// Draft mode: push each spec as a preview version without touching the live definition.
	if draft {
		return applyDashboardsAsDraft(c, entries, previewDataset)
	}

	// Fetch existing definitions (all orgs; filtered by slug match below).
	existingBySlug, err := fetchExistingDefs(c.base, c.token)
	if err != nil {
		return 0, fmt.Errorf("fetch existing dashboard definitions: %w", err)
	}

	var created, updated, noop, drift int
	for _, path := range entries {
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		spec, err := loadSpec(path)
		if err != nil {
			return drift, fmt.Errorf("load %s: %w", path, err)
		}
		slug, _ := spec["slug"].(string)
		if slug == "" {
			fmt.Fprintf(os.Stderr, "provision: SKIP dashboard spec missing slug: %s\n", path)
			continue
		}

		// Inject provisioned fields — always overwrite so specs stay portable.
		spec["org_id"] = orgID
		if providerID > 0 {
			spec["provider_id"] = providerID
		}
		if draft {
			spec["draft"] = true
		}
		if previewDataset != "" {
			spec["preview_dataset"] = previewDataset
		}

		existing, exists := existingBySlug[slug]
		if c.dryRun {
			if exists {
				fmt.Printf("  [dry-run] UPDATE %-45s (id=%d)\n", slug, existing.ID)
				drift++
			} else {
				fmt.Printf("  [dry-run] CREATE %s\n", slug)
			}
			continue
		}

		if exists {
			// Check whether anything actually changed by comparing the marshalled spec
			// against the stored definition fields we can compare. We do a lightweight
			// comparison: if the spec's org_id/provider_id/slug match and the full spec
			// JSON differs from what the server would return, call it drift and update.
			//
			// Simpler than the full diffFields approach from the standalone provision
			// tool — we don't have a detail endpoint here, so we always PUT on update.
			// This is still idempotent: PUT is version-snapshotted server-side.
			fmt.Printf("  UPDATE %-45s (id=%d)\n", slug, existing.ID)
			if err := updateDefinition(c.base, c.token, existing.ID, spec); err != nil {
				return drift, fmt.Errorf("update dashboard %q: %w", slug, err)
			}
			updated++
			drift++
		} else {
			fmt.Printf("  CREATE %s\n", slug)
			id, err := createDefinition(c.base, c.token, spec)
			if err != nil {
				return drift, fmt.Errorf("create dashboard %q: %w", slug, err)
			}
			fmt.Printf("  CREATE %-45s ok (id=%d)\n", slug, id)
			created++
		}
	}
	_ = noop // reported implicitly via absence in summary

	fmt.Printf("provision: dashboards summary: created=%d updated=%d drift=%d\n", created, updated, drift)
	return drift, nil
}

// applyDashboardsAsDraft pushes each dashboard spec as a preview version
// (POST /admin/dashboards/{slug}/preview) without touching the live definition.
func applyDashboardsAsDraft(c *provisionClient, entries []string, previewDataset string) (int, error) {
	var count int
	for _, path := range entries {
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return count, fmt.Errorf("read %s: %w", path, err)
		}
		var spec dashboardSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			return count, fmt.Errorf("parse %s: %w", path, err)
		}
		slug, _ := spec["slug"].(string)
		if slug == "" {
			fmt.Fprintf(os.Stderr, "provision: SKIP draft spec missing slug: %s\n", path)
			continue
		}

		fmt.Printf("  DRAFT  %s\n", slug)
		if c.dryRun {
			count++
			continue
		}

		payload, _ := json.Marshal(map[string]any{
			"spec":               string(raw),
			"preview_dataset_id": previewDataset,
		})
		respBody, status, err := apiCall("POST",
			c.base+"/api/admin/dashboards/"+slug+"/preview",
			c.token, payload)
		if err != nil || status >= 300 {
			return count, fmt.Errorf("draft %q: status=%d err=%v body=%s",
				slug, status, err, provisionSummarize(respBody))
		}
		count++
	}
	fmt.Printf("provision: dashboards draft summary: drafted=%d\n", count)
	return count, nil
}
