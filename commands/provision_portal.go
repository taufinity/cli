package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type portalConfig struct {
	PortalName   string   `yaml:"portal_name"`
	PortalDomain string   `yaml:"portal_domain"`
	PrimaryColor string   `yaml:"primary_color"`
	AccentColor  string   `yaml:"accent_color"`
	ChartColors  []string `yaml:"chart_colors"`
	// ForceTheme locks portal users to a specific theme regardless of their
	// personal preference. Valid values: "light", "dark", or "" (unset = honor
	// user preference). Used by customers whose dashboards were designed against
	// a specific palette (e.g. Felix mockups are light-theme).
	ForceTheme string `yaml:"force_theme"`

	// DefaultPermissionScopes lists named permission scopes (e.g. "mcp") that
	// every non-admin key in this org should carry. On provision, the tool
	// idempotently posts each scope to each existing non-admin key in the org.
	// Already-applied scopes are no-ops (HTTP 200 already_existed=true).
	// Permissions are only ever ADDED by this mechanism, never removed —
	// scopes a customer added via the Studio UI are preserved. Phase 2 (2026-05-06).
	DefaultPermissionScopes []string `yaml:"default_permission_scopes"`

	// Managed AI billing — optional, system-admin token required.
	// Omit from portal.yaml to leave the org's current values unchanged.
	ManagedAI        *bool    `yaml:"managed_ai,omitempty"`
	MonthlyBudgetUSD *float64 `yaml:"monthly_budget_usd,omitempty"`
	DailyBudgetUSD   *float64 `yaml:"daily_budget_usd,omitempty"`
	AIMarkupPct      *float64 `yaml:"ai_markup_pct,omitempty"`
}

// upsertPortal applies portal branding and domain settings via PATCH /api/organizations/{id}.
func upsertPortal(c *provisionClient, orgID uint, cfg portalConfig) error {
	// Validate force_theme client-side so we fail fast with a clear message
	// instead of bouncing off the server validator.
	if cfg.ForceTheme != "" && cfg.ForceTheme != "light" && cfg.ForceTheme != "dark" {
		return fmt.Errorf("portal force_theme must be \"light\", \"dark\", or empty (got %q)", cfg.ForceTheme)
	}

	body := map[string]interface{}{
		"portal_name":   cfg.PortalName,
		"portal_domain": cfg.PortalDomain,
		"primary_color": cfg.PrimaryColor,
		"accent_color":  cfg.AccentColor,
		"chart_colors":  cfg.ChartColors,
		"force_theme":   cfg.ForceTheme,
	}
	if cfg.ManagedAI != nil {
		body["managed_ai"] = *cfg.ManagedAI
	}
	if cfg.MonthlyBudgetUSD != nil {
		body["monthly_budget_usd"] = *cfg.MonthlyBudgetUSD
	}
	if cfg.DailyBudgetUSD != nil {
		body["daily_budget_usd"] = *cfg.DailyBudgetUSD
	}
	if cfg.AIMarkupPct != nil {
		body["ai_markup_pct"] = *cfg.AIMarkupPct
	}
	payload, _ := json.Marshal(body)
	fmt.Printf("provision: updating portal for org %d (domain=%q force_theme=%q managed_ai=%v)\n", orgID, cfg.PortalDomain, cfg.ForceTheme, cfg.ManagedAI)
	_, status, err := c.patch(fmt.Sprintf("/organizations/%d", orgID), payload)
	if err != nil || status >= 300 {
		return fmt.Errorf("update portal: status=%d err=%v", status, err)
	}
	return nil
}

// ensureDefaultPermissionScopes walks every non-admin API key in the named org
// and idempotently posts each scope from cfg.DefaultPermissionScopes. The
// scope-add endpoint is itself idempotent (200 already_existed=true on a row
// that already exists), so this is safe to run on every provision and never
// silently mutates customer-added permissions.
//
// Failures are *collected and reported at the end* rather than failing fast —
// one bad scope on one key shouldn't hide problems on the others. The
// function returns a single error containing all collected failures only at
// the end of the loop.
func ensureDefaultPermissionScopes(c *provisionClient, orgID uint, cfg portalConfig) error {
	if len(cfg.DefaultPermissionScopes) == 0 {
		return nil
	}

	// Use the org_id query param so we don't pull every key in the system on
	// every customer's provision run (cheap server-side filter; keys for
	// other orgs aren't even serialized).
	body, status, err := c.get(fmt.Sprintf("/admin/api-keys?organization_id=%d", orgID))
	if err != nil || status >= 300 {
		return fmt.Errorf("list api keys: status=%d err=%v", status, err)
	}
	var keys []struct {
		ID             uint  `json:"id"`
		IsAdmin        bool  `json:"is_admin"`
		OrganizationID *uint `json:"organization_id"`
		Active         bool  `json:"active"`
	}
	if err := json.Unmarshal(body, &keys); err != nil {
		return fmt.Errorf("decode api keys: %w", err)
	}

	scoped := 0
	var failures []string
	for _, k := range keys {
		if k.IsAdmin || !k.Active {
			continue
		}
		// Server filters by org now, but defensive: the server is the source
		// of truth, not the client. Skip anything that doesn't match.
		if k.OrganizationID == nil || *k.OrganizationID != orgID {
			continue
		}
		for _, scope := range cfg.DefaultPermissionScopes {
			payload, _ := json.Marshal(map[string]string{"scope": scope})
			_, st, err := c.post(fmt.Sprintf("/admin/api-keys/%d/permissions", k.ID), payload)
			if err != nil || st >= 300 {
				failures = append(failures, fmt.Sprintf("scope %q on key %d: status=%d err=%v", scope, k.ID, st, err))
				continue
			}
			scoped++
		}
	}
	fmt.Printf("provision: ensured %d default-scope row(s) across non-admin keys in org %d\n", scoped, orgID)
	if len(failures) > 0 {
		return fmt.Errorf("ensure default scopes: %d failure(s):\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

func applyPortal(c *provisionClient, dir string, orgID uint) error {
	pf := filepath.Join(dir, "portal.yaml")
	if !fileExists(pf) {
		return nil
	}
	var cfg portalConfig
	mustReadYAML(pf, &cfg)
	if err := upsertPortal(c, orgID, cfg); err != nil {
		return err
	}
	if len(cfg.DefaultPermissionScopes) > 0 {
		if err := ensureDefaultPermissionScopes(c, orgID, cfg); err != nil {
			c.Warn("default permission scopes: %v", err)
		}
	}
	return nil
}
