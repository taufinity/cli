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
// actually differ from the server (drift count), which the caller uses for
// --strict exit codes.
//
// Existing dashboards are diffed field by field against a detail GET before any
// write: if nothing changed, the dashboard is a NOOP and no PUT is issued. That
// keeps apply from rewriting every row on every run, and from silently reverting
// a UI edit that nobody pulled back into the spec yet.
//
// Deletion happens only for slugs named in dashboards/_tombstones.json — see
// provision_dashboards_tombstones.go for why absence from the directory is
// deliberately not enough.
//
// Dry-run is handled by the client: writes are printed, not sent.
//
// providerID is the default (the primary/root provider.yaml); providersBySlug
// resolves a dashboard's own "provider" field (a providerConfig.Slug) to a
// specific provider's ID for directories with more than one BQ provider.
//
// TODO: diffFields (below) does not compare provider_id, so an existing
// dashboard whose "provider" field changes but nothing else does will NOOP
// instead of updating — the resolved ID isn't threaded into the diff today.
// Not hit by a brand-new dashboard (CREATE path), only by re-pointing one
// that already exists.
func applyDashboards(c *provisionClient, dir string, orgID, providerID uint, providersBySlug map[string]uint, draft bool, previewDataset string) (int, error) {
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

	existing, err := listDashboardDefs(c, orgID)
	if err != nil {
		return 0, err
	}
	bySlug := make(map[string]provisionDashboardDef, len(existing))
	for _, d := range existing {
		bySlug[d.Slug] = d
	}

	// Tombstones run before the upserts so that a slug being deleted and then
	// re-created under the same name (the supported way to rebuild a dashboard
	// from scratch) resolves in that order rather than the reverse.
	deleted, err := applyDashboardTombstones(c, dashDir, bySlug)
	if err != nil {
		return 0, err
	}

	var created, updated, noop, drift int
	for _, path := range entries {
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return drift, fmt.Errorf("read %s: %w", path, err)
		}
		var local provisionDashboardDef
		if err := json.Unmarshal(raw, &local); err != nil {
			return drift, fmt.Errorf("parse %s: %w", path, err)
		}
		if local.Slug == "" {
			fmt.Fprintf(os.Stderr, "provision: SKIP dashboard spec missing slug: %s\n", path)
			continue
		}

		resolvedProviderID := providerID
		if local.Provider != "" {
			id, ok := providersBySlug[local.Provider]
			if !ok {
				return drift, fmt.Errorf("dashboard %q: provider %q not found (check providers/*.yaml has a matching slug)", local.Slug, local.Provider)
			}
			resolvedProviderID = id
		}

		payload, _ := json.Marshal(map[string]any{
			"org_id":               orgID,
			"provider_id":          resolvedProviderID,
			"slug":                 local.Slug,
			"name":                 local.Name,
			"description":          local.Description,
			"source_view":          local.SourceView,
			"columns":              rawStrOrNull(local.Columns),
			"filters":              rawStrOrNull(local.Filters),
			"default_chart":        local.DefaultChart,
			"default_sort":         rawStrOrNull(local.DefaultSort),
			"layout":               rawStrOrNull(local.Layout),
			"max_rows":             local.MaxRows,
			"position":             local.Position,
			"static_filters":       rawStrOrNull(local.normalizedStaticFilters()),
			"hidden_from_overview": local.HiddenFromOverview,
			"export_enabled":       local.ExportEnabled,
			"client_group_filter":  rawStrOrNull(local.ClientGroupFilter),
		})

		cur, exists := bySlug[local.Slug]
		if !exists {
			fmt.Printf("CREATE %s\n", local.Slug)
			body, status, err := c.post("/admin/dashboard-definitions", payload)
			if err != nil || status >= 300 {
				return drift, provisionAPIErr(fmt.Sprintf("create dashboard %q", local.Slug), status, body, err)
			}
			created++
			continue
		}

		detail, err := getDashboardDetail(c, orgID, cur.ID)
		if err != nil {
			return drift, fmt.Errorf("get detail for %q: %w", local.Slug, err)
		}
		diffs := diffFields(local, *detail)
		if len(diffs) == 0 {
			fmt.Printf("NOOP   %s id=%d\n", local.Slug, cur.ID)
			noop++
			continue
		}
		fmt.Printf("UPDATE %s id=%d [%s]\n", local.Slug, cur.ID, strings.Join(diffs, ", "))
		drift++
		body, status, err := c.put(fmt.Sprintf("/admin/dashboard-definitions/%d", cur.ID), payload)
		if err != nil || status >= 300 {
			return drift, provisionAPIErr(fmt.Sprintf("update dashboard %q", local.Slug), status, body, err)
		}
		updated++
	}

	fmt.Printf("provision: dashboards summary: created=%d updated=%d deleted=%d noop=%d drift=%d\n",
		created, updated, deleted, noop, drift)
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

		payload, _ := json.Marshal(map[string]any{
			"spec":               string(raw),
			"preview_dataset_id": previewDataset,
		})
		respBody, status, err := c.post("/admin/dashboards/"+slug+"/preview", payload)
		if err != nil || status >= 300 {
			return count, provisionAPIErr(fmt.Sprintf("draft dashboard %q", slug), status, respBody, err)
		}
		count++
	}
	fmt.Printf("provision: dashboards draft summary: drafted=%d\n", count)
	return count, nil
}
