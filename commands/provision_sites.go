package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// site.go — site resolution and creation
// ---------------------------------------------------------------------------

// siteYAML mirrors the on-disk shape of sites/<dir>/site.yaml.
//
// Two forms are accepted:
//
//	site_id: "quizreps_com"   # preferred — env-portable, resolved via API
//	id: 6                      # legacy — numeric primary key, env-specific
//
// If both are set, provision refuses to apply (ambiguous). If neither is set,
// the directory name is used as the site_id.
type siteYAML struct {
	SiteID string `yaml:"site_id"`
	ID     uint   `yaml:"id"`
	// Name is used when provision creates a site that does not yet exist.
	Name string `yaml:"name,omitempty"`
	// CategoryPages is SSG-only; captured here so the strict YAML decoder does
	// not reject site.yaml files that include it.
	CategoryPages any `yaml:"category_pages,omitempty"`
}

// siteRecord is the trimmed payload returned by GET /api/sites/by-site-id/{site_id}.
type siteRecord struct {
	ID             uint   `json:"id"`
	SiteID         string `json:"site_id"`
	Name           string `json:"name"`
	OrganizationID *uint  `json:"organization_id"`
}

// resolveSiteID converts a string site_id to the numeric primary key.
// found is false (with nil error) when the site does not exist yet.
func resolveSiteID(c *provisionClient, orgID uint, siteIDStr string) (id uint, found bool, err error) {
	path := "/sites/by-site-id/" + siteIDStr
	body, status, err := c.get(path)
	if err != nil {
		return 0, false, fmt.Errorf("lookup site_id %q: %w", siteIDStr, err)
	}
	if status == 404 {
		return 0, false, nil
	}
	if status != 200 {
		return 0, false, fmt.Errorf("lookup site_id %q: status=%d body=%s", siteIDStr, status, string(body))
	}
	var rec siteRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return 0, false, fmt.Errorf("parse site lookup response: %w", err)
	}
	if rec.OrganizationID == nil {
		return 0, false, fmt.Errorf("site %q has no organization — refusing to provision", siteIDStr)
	}
	if *rec.OrganizationID != orgID {
		return 0, false, fmt.Errorf(
			"site %q belongs to org %d but provision targets org %d — refusing to cross-write",
			siteIDStr, *rec.OrganizationID, orgID,
		)
	}
	return rec.ID, true, nil
}

// createSite creates a site in the given org via POST /api/sites.
// In dry-run mode no site is created and 0 is returned.
func createSite(c *provisionClient, orgID uint, siteID, name string) (uint, error) {
	if name == "" {
		name = siteID
	}
	fmt.Printf("CREATE site site_id=%q name=%q org=%d\n", siteID, name, orgID)
	payload, _ := json.Marshal(map[string]any{
		"site_id":         siteID,
		"name":            name,
		"organization_id": orgID,
	})
	body, status, err := c.writeForOrg("POST", "/sites/", payload, orgID)
	if err != nil || status >= 300 {
		return 0, provisionAPIErr(fmt.Sprintf("create site %q", siteID), status, body, err)
	}
	if c.dryRun {
		return 0, nil
	}
	var rec siteRecord
	if err := json.Unmarshal(body, &rec); err != nil || rec.ID == 0 {
		return 0, fmt.Errorf("create site %q: response missing id (body=%s)", siteID, provisionSummarize(body))
	}
	return rec.ID, nil
}

// loadSiteYAML reads sites/<dir>/site.yaml if present. Missing file returns an
// empty struct (caller falls back to dir name).
func loadSiteYAML(path string) (siteYAML, error) {
	var sy siteYAML
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return sy, nil
	} else if err != nil {
		return sy, err
	}
	mustReadYAML(path, &sy)
	return sy, nil
}

// resolveSiteFromDir picks the right numeric site ID for the given site directory.
// Resolution order:
//  1. site.yaml with site_id (string) → API lookup (preferred)
//  2. site.yaml with id (uint, legacy) → use directly
//  3. No site.yaml → directory name as site_id → API lookup
func resolveSiteFromDir(c *provisionClient, orgID uint, dirName, sitesDir string) (uint, string, error) {
	sf := filepath.Join(sitesDir, dirName, "site.yaml")
	sy, err := loadSiteYAML(sf)
	if err != nil {
		return 0, "", fmt.Errorf("read %s: %w", sf, err)
	}

	if sy.SiteID != "" && sy.ID != 0 {
		return 0, "", fmt.Errorf(
			"site %q: site.yaml has both `site_id` and `id` set — pick one (prefer site_id)",
			dirName,
		)
	}

	if sy.SiteID != "" {
		id, found, err := resolveSiteID(c, orgID, sy.SiteID)
		if err != nil {
			return 0, "", err
		}
		if !found {
			id, err = createSite(c, orgID, sy.SiteID, sy.Name)
			if err != nil {
				return 0, "", err
			}
			return id, fmt.Sprintf("created site_id=%q", sy.SiteID), nil
		}
		return id, fmt.Sprintf("site_id=%q", sy.SiteID), nil
	}

	if sy.ID != 0 {
		fmt.Printf("provision: WARN site %q: site.yaml uses legacy `id: %d` — "+
			"prefer `site_id: <string>` for env-portable config\n", dirName, sy.ID)
		return sy.ID, fmt.Sprintf("legacy id=%d", sy.ID), nil
	}

	// Fall back to directory name as the site_id.
	id, found, err := resolveSiteID(c, orgID, dirName)
	if err != nil {
		return 0, "", fmt.Errorf("site %q: no site.yaml and dir name lookup failed: %w", dirName, err)
	}
	if !found {
		id, err = createSite(c, orgID, dirName, sy.Name)
		if err != nil {
			return 0, "", fmt.Errorf("site %q: not found and create failed: %w", dirName, err)
		}
		return id, fmt.Sprintf("created dir-name site_id=%q", dirName), nil
	}
	return id, fmt.Sprintf("dir-name site_id=%q", dirName), nil
}

// ---------------------------------------------------------------------------
// pipeline.go — pipeline upsert and provider reference resolution
// ---------------------------------------------------------------------------

type pipelineStep struct {
	Name      string                 `yaml:"name"       json:"name"`
	Type      string                 `yaml:"type"       json:"type,omitempty"`
	Enabled   *bool                  `yaml:"enabled"    json:"enabled,omitempty"`
	Provider  string                 `yaml:"provider"   json:"provider,omitempty"`
	Model     string                 `yaml:"model"      json:"model,omitempty"`
	OutputKey string                 `yaml:"output_key" json:"output_key,omitempty"`
	Settings  map[string]interface{} `yaml:"settings"   json:"settings,omitempty"`
}

type pipelineConfig struct {
	Steps []pipelineStep `yaml:"steps"`
}

// resolveProvisionProviderID looks up the numeric ID of a custom AI provider
// within an org. When slug is non-empty it matches by slug (env-independent);
// otherwise falls back to case-insensitive name matching.
//
// Named resolveProvisionProviderID to avoid collision with resolveProviderID in
// dashboards.go (same package, different signature).
func resolveProvisionProviderID(c *provisionClient, orgID uint, name, slug string) (uint, error) {
	body, status, err := c.getForOrg("/custom-ai-providers", orgID)
	if err != nil || status != 200 {
		return 0, fmt.Errorf("list providers: status=%d err=%v", status, err)
	}
	type providerRow struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug,omitempty"`
	}
	var items []providerRow
	if err := json.Unmarshal(body, &items); err != nil {
		var wrapped struct {
			Data []providerRow `json:"data"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return 0, fmt.Errorf("parse providers: %w", err)
		}
		items = wrapped.Data
	}
	for _, p := range items {
		if slug != "" {
			if p.Slug == slug {
				return p.ID, nil
			}
			continue
		}
		if strings.EqualFold(p.Name, name) {
			return p.ID, nil
		}
	}
	if slug != "" {
		return 0, fmt.Errorf("provider with slug %q not found", slug)
	}
	return 0, fmt.Errorf("provider %q not found", name)
}

// resolveProviderRefs rewrites `provider: "custom:<Name>"` to `provider: "custom:<id>"`
// and resolves `settings.provider_id: "<Name>"` to a numeric ID.
// Strings that are already numeric IDs pass through unchanged.
func resolveProviderRefs(c *provisionClient, orgID uint, steps []pipelineStep) error {
	resolve := func(name string) (uint, error) {
		if name == "" {
			return 0, nil
		}
		if id, err := strconv.ParseUint(name, 10, 32); err == nil {
			return uint(id), nil
		}
		return resolveProvisionProviderID(c, orgID, name, "")
	}

	for i, step := range steps {
		// Skip disabled steps — they won't be called at runtime.
		if step.Enabled != nil && !*step.Enabled {
			continue
		}

		// Provider field: "custom:<name>" → "custom:<id>"
		if strings.HasPrefix(step.Provider, "custom:") {
			ref := strings.TrimPrefix(step.Provider, "custom:")
			if _, err := strconv.Atoi(ref); err != nil {
				id, lookupErr := resolveProvisionProviderID(c, orgID, ref, "")
				if lookupErr != nil {
					return fmt.Errorf("step %q: resolve provider %q: %w", step.Name, ref, lookupErr)
				}
				originalRef := step.Provider
				steps[i].Provider = fmt.Sprintf("custom:%d", id)
				fmt.Printf("provision: resolved %s → custom:%d (step=%s)\n", originalRef, id, step.Name)
			}
		}

		// Settings.provider_id: accepts numeric or name (string)
		if step.Settings != nil {
			if raw, ok := step.Settings["provider_id"]; ok {
				if s, isStr := raw.(string); isStr {
					id, err := resolve(s)
					if err != nil {
						return fmt.Errorf("step %q: resolve provider_id %q: %w", step.Name, s, err)
					}
					steps[i].Settings["provider_id"] = id
				}
			}
		}
	}
	return nil
}

// upsertPipeline applies the given steps to the site's pipeline using mode=replace,
// which is idempotent — it replaces the entire pipeline rather than merging.
func upsertPipeline(c *provisionClient, siteID uint, steps []pipelineStep) error {
	path := fmt.Sprintf("/sites/%d/pipeline", siteID)

	// Verify site exists before writing.
	_, status, err := c.get(path)
	if err != nil || status != 200 {
		return fmt.Errorf("get pipeline for site %d: status=%d err=%v", siteID, status, err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"mode":  "replace",
		"steps": steps,
	})
	fmt.Printf("provision: updating pipeline for site %d (%d steps)\n", siteID, len(steps))
	_, status, err = c.put(path, payload)
	if err != nil || status >= 300 {
		return fmt.Errorf("update pipeline for site %d: status=%d err=%v", siteID, status, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// secure_render.go — secure render settings
// ---------------------------------------------------------------------------

type secureRenderConfig struct {
	Enabled         bool   `yaml:"secure_render_enabled"`
	TemplateRepoURL string `yaml:"template_repo_url"`
}

type gitRepo struct {
	ID      uint   `json:"id"`
	RepoURL string `json:"repo_url"`
}

// upsertSecureRender resolves the repo URL to a git_repo_connections ID,
// then applies secure render settings via PUT /api/sites/{id}/settings/secure-render.
func upsertSecureRender(c *provisionClient, orgID uint, siteID uint, cfg secureRenderConfig) error {
	repoID, err := resolveRepoID(c, orgID, cfg.TemplateRepoURL)
	if err != nil {
		return fmt.Errorf("resolve repo URL %q: %w", cfg.TemplateRepoURL, err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"secure_render_enabled": cfg.Enabled,
		"template_repo_id":      repoID,
	})
	fmt.Printf("provision: updating secure-render for site %d (repo_id=%d)\n", siteID, repoID)
	_, status, err := c.put(fmt.Sprintf("/sites/%d/settings/secure-render", siteID), payload)
	if err != nil || status >= 300 {
		return fmt.Errorf("update secure-render: status=%d err=%v", status, err)
	}
	return nil
}

// resolveRepoID finds the git_repo_connections.id matching the given URL for the org.
func resolveRepoID(c *provisionClient, orgID uint, repoURL string) (uint, error) {
	body, status, err := c.get(fmt.Sprintf("/organizations/%d/git-repos", orgID))
	if err != nil || status != 200 {
		return 0, fmt.Errorf("list git repos: status=%d err=%v", status, err)
	}
	var repos []gitRepo
	if err := json.Unmarshal(body, &repos); err != nil {
		return 0, fmt.Errorf("parse repos: %w", err)
	}
	for _, r := range repos {
		if strings.EqualFold(r.RepoURL, repoURL) {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("no git repo found with URL %q in org %d", repoURL, orgID)
}

// ---------------------------------------------------------------------------
// ai_settings.go — per-site AI settings
// ---------------------------------------------------------------------------

type aiSettingsConfig struct {
	Model           string  `yaml:"model"            json:"model,omitempty"`
	Temperature     float64 `yaml:"temperature"      json:"temperature,omitempty"`
	MaxTokens       int     `yaml:"max_tokens"       json:"max_tokens,omitempty"`
	SystemPrompt    string  `yaml:"system_prompt"    json:"system_prompt,omitempty"`
	PromptPrefix    string  `yaml:"prompt_prefix"    json:"prompt_prefix,omitempty"`
	PromptSuffix    string  `yaml:"prompt_suffix"    json:"prompt_suffix,omitempty"`
	SafetyModel     string  `yaml:"safety_model"     json:"safety_model,omitempty"`
	RefinementModel string  `yaml:"refinement_model" json:"refinement_model,omitempty"`
	MinHumanScore   int     `yaml:"min_human_score"  json:"min_human_score,omitempty"`
}

func provisionAISettings(c *provisionClient, siteID uint, siteDir string) error {
	path := siteDir + "/ai-settings.yaml"
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ai-settings.yaml: %w", err)
	}

	var cfg aiSettingsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse ai-settings.yaml: %w", err)
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal ai settings: %w", err)
	}

	apiPath := fmt.Sprintf("/sites/%d/settings/ai", siteID)
	if _, _, err := c.put(apiPath, payload); err != nil {
		return fmt.Errorf("update AI settings for site %d: %w", siteID, err)
	}

	fmt.Printf("provision: site %d AI settings updated\n", siteID)
	return nil
}

// ---------------------------------------------------------------------------
// Top-level orchestration
// ---------------------------------------------------------------------------

// applySiteDir provisions all resources for a single site directory:
// pipeline, secure-render, and AI settings.
func applySiteDir(c *provisionClient, siteDir string, orgID uint) error {
	dirName := filepath.Base(siteDir)
	sitesDir := filepath.Dir(siteDir)

	hasPipeline := fileExists(filepath.Join(siteDir, "pipeline.yaml"))
	hasSecureRender := fileExists(filepath.Join(siteDir, "secure-render.yaml"))
	hasAISettings := fileExists(filepath.Join(siteDir, "ai-settings.yaml"))

	if !hasPipeline && !hasSecureRender && !hasAISettings {
		return nil
	}

	// Resolve numeric site ID only when at least one resource needs it.
	siteID, source, err := resolveSiteFromDir(c, orgID, dirName, sitesDir)
	if err != nil {
		return err
	}
	fmt.Printf("provision: site %q → id %d (%s)\n", dirName, siteID, source)

	if hasPipeline {
		var cfg pipelineConfig
		mustReadYAML(filepath.Join(siteDir, "pipeline.yaml"), &cfg)
		if err := resolveProviderRefs(c, orgID, cfg.Steps); err != nil {
			return fmt.Errorf("pipeline: %w", err)
		}
		if err := upsertPipeline(c, siteID, cfg.Steps); err != nil {
			return fmt.Errorf("pipeline: %w", err)
		}
	}

	if hasSecureRender {
		var cfg secureRenderConfig
		mustReadYAML(filepath.Join(siteDir, "secure-render.yaml"), &cfg)
		if err := upsertSecureRender(c, orgID, siteID, cfg); err != nil {
			return fmt.Errorf("secure-render: %w", err)
		}
	}

	if hasAISettings {
		if err := provisionAISettings(c, siteID, siteDir); err != nil {
			return fmt.Errorf("ai-settings: %w", err)
		}
	}

	return nil
}

// applySites walks the sites/ subdirectory and provisions each site directory.
func applySites(c *provisionClient, dir string, orgID uint) error {
	sitesDir := filepath.Join(dir, "sites")
	if !fileExists(sitesDir) {
		return nil
	}
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		return fmt.Errorf("sites/: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		siteDir := filepath.Join(sitesDir, e.Name())
		if err := applySiteDir(c, siteDir, orgID); err != nil {
			return fmt.Errorf("site %s: %w", e.Name(), err)
		}
	}
	return nil
}
