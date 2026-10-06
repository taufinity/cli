package commands

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type providerConfig struct {
	// ID pins the provider to a specific Studio record. When non-zero, provision
	// matches by ID instead of name, preventing accidental duplicates if the name
	// drifts. After first creation the tool writes back the assigned ID here.
	ID int `yaml:"id,omitempty"`

	// SkipUpsert skips the create/update step for this provider. Used for
	// system-level providers (organization_id=null) that are not returned by the
	// org-scoped /api/custom-ai-providers endpoint and therefore cannot be managed
	// via the normal upsert flow. When set alongside id, that id is used directly
	// as the provider_id for dashboard association — no API lookup needed.
	SkipUpsert bool `yaml:"skip_upsert,omitempty"`

	Name string `yaml:"name"`
	// Slug is a stable, env-independent identifier. When set, provision matches
	// the provider by slug instead of id/name — robust across environments where
	// the numeric id differs and names may be ambiguous.
	Slug          string   `yaml:"slug,omitempty"`
	Description   string   `yaml:"description"`
	ProviderType  string   `yaml:"provider_type"`
	Category      string   `yaml:"category"`
	EndpointURL   string   `yaml:"endpoint_url"`
	HTTPMethod    string   `yaml:"http_method"`
	AllowedTables []string `yaml:"allowed_tables"`
	// TablePages maps an allow-listed table name to the path of its table
	// page (markdown, ADR-016 rule 8), relative to --repo-root (the parent of
	// --dir by default); a ../ path into a sibling repo is allowed, so a page
	// can live where dbt's meta.table_page points. Provision uploads the
	// CONTENT, so the pages are provisioned, never hand-edited in Studio.
	TablePages map[string]string `yaml:"table_pages,omitempty"`
	// tablePageContents is the resolved content of TablePages, filled by
	// applyProviders before upsert; not a YAML field.
	tablePageContents map[string]resolvedTablePage `yaml:"-"`
	MaxBytesBilled    int64                        `yaml:"max_bytes_billed"`
	Enabled           bool                         `yaml:"enabled"`

	// REST-provider fields (provider_type != bigquery)
	MessageParamName string `yaml:"message_param_name,omitempty"`
	AuthParamName    string `yaml:"auth_param_name,omitempty"`
	// AuthParamValue should ideally reference a secret/env var, not raw value if possible
	AuthParamValue   string            `yaml:"auth_param_value,omitempty"`
	InputTemplate    string            `yaml:"input_template,omitempty"`
	ResponseMappings map[string]string `yaml:"response_mappings,omitempty"`
	ResponseJSONPath string            `yaml:"response_json_path,omitempty"`
	RequestHeaders   map[string]string `yaml:"request_headers,omitempty"`
	RequestTimeout   int               `yaml:"request_timeout,omitempty"`
	MaxRetries       int               `yaml:"max_retries,omitempty"`
	RateLimitPerMin  int               `yaml:"rate_limit_per_min,omitempty"`
}

// upsertProvider creates or updates a provider for the given org.
// When cfg.ID > 0 it matches by ID (safe rename support); otherwise falls back
// to case-insensitive name match.
// Returns the live provider ID (existing or newly created) so the caller can
// write it back to the YAML file as a pinned id.
func upsertProvider(c *provisionClient, orgID uint, cfg providerConfig) (int, error) {
	if err := validateProviderBoundary(cfg); err != nil {
		return 0, err
	}
	body, status, err := c.getForOrg("/custom-ai-providers", orgID)
	if err != nil || status != 200 {
		return 0, fmt.Errorf("list providers: status=%d err=%v", status, err)
	}

	// API returns plain array
	var items []providerItem
	if err := json.Unmarshal(body, &items); err != nil {
		// Try wrapped form {"data":[...]}
		var wrapped struct {
			Data []providerItem `json:"data"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return 0, fmt.Errorf("parse list: %w", err)
		}
		items = wrapped.Data
	}

	isBQ := strings.EqualFold(cfg.ProviderType, "bigquery")

	// Base payload — only fields common to all provider types.
	payload := map[string]interface{}{
		"name":          cfg.Name,
		"description":   cfg.Description,
		"provider_type": cfg.ProviderType,
		"category":      cfg.Category,
		"endpoint_url":  cfg.EndpointURL,
		"http_method":   cfg.HTTPMethod,
		"enabled":       cfg.Enabled,
	}

	// Only send slug when the YAML sets one — avoids clearing the slug on
	// legacy (slugless) provider configs.
	if cfg.Slug != "" {
		payload["slug"] = cfg.Slug
	}

	// BQ-specific fields that the REST handler rejects.
	var allowedJSON []byte
	if isBQ {
		allowedTables := cfg.AllowedTables
		if allowedTables == nil {
			allowedTables = []string{}
		}
		allowedJSON, err = json.Marshal(allowedTables)
		if err != nil {
			return 0, fmt.Errorf("marshal allowed_tables: %w", err)
		}
		payload["allowed_tables"] = string(allowedJSON)
		payload["max_bytes_billed"] = cfg.MaxBytesBilled
	}

	// REST-specific fields.
	if cfg.MessageParamName != "" {
		payload["message_param_name"] = cfg.MessageParamName
	}
	if cfg.AuthParamName != "" {
		payload["auth_param_name"] = cfg.AuthParamName
	}
	if cfg.AuthParamValue != "" {
		payload["auth_param_value"] = cfg.AuthParamValue
	}
	if cfg.InputTemplate != "" {
		payload["input_template"] = cfg.InputTemplate
	}
	// Map fields: nil (key absent or null in YAML) keeps the stored value; an
	// explicit `{}` is sent as "{}", which Studio stores and reads back as an
	// empty map, i.e. it clears the stored value. Studio ignores an empty
	// string on update, so "{}" is the only way to clear these.
	if cfg.ResponseMappings != nil {
		mappingsJSON, err := json.Marshal(cfg.ResponseMappings)
		if err != nil {
			return 0, fmt.Errorf("marshal response_mappings: %w", err)
		}
		payload["response_mappings"] = string(mappingsJSON)
	}
	if cfg.ResponseJSONPath != "" {
		payload["response_json_path"] = cfg.ResponseJSONPath
	}
	if cfg.RequestHeaders != nil {
		headersJSON, err := json.Marshal(cfg.RequestHeaders)
		if err != nil {
			return 0, fmt.Errorf("marshal request_headers: %w", err)
		}
		payload["request_headers"] = string(headersJSON)
	}
	if cfg.RequestTimeout > 0 {
		payload["request_timeout"] = cfg.RequestTimeout
	}
	if cfg.MaxRetries > 0 {
		payload["max_retries"] = cfg.MaxRetries
	}
	if cfg.RateLimitPerMin > 0 {
		payload["rate_limit_per_min"] = cfg.RateLimitPerMin
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	// Match existing provider: slug (when both sides have one) → id → name.
	// When cfg has a slug but the existing provider doesn't yet (bootstrap case),
	// fall through to id/name so the first run writes the slug into the PUT
	// payload. Subsequent runs then match by slug.
	for _, existing := range items {
		var matched bool
		if cfg.Slug != "" && existing.Slug != "" {
			matched = existing.Slug == cfg.Slug
		} else if cfg.ID > 0 {
			matched = existing.ID == cfg.ID
		} else {
			matched = strings.EqualFold(existing.Name, cfg.Name)
		}
		if !matched {
			continue
		}
		if cfg.ID > 0 && !strings.EqualFold(existing.Name, cfg.Name) {
			fmt.Printf("provision: provider id=%d name changed %q → %q\n", existing.ID, existing.Name, cfg.Name)
		}
		fmt.Printf("provision: updating provider %q (id=%d)\n", cfg.Name, existing.ID)
		// The dry-run payload line is truncated, so spell out map-field changes
		// (a clear in particular) for provision diff.
		for _, change := range providerMapFieldChanges(cfg, existing) {
			fmt.Printf("  %s\n", change)
		}
		diffTablePages(c, existing.ID, cfg.tablePageContents)
		_, status, err = c.put(fmt.Sprintf("/custom-ai-providers/%d", existing.ID), payloadBytes)
		if err != nil || status >= 300 {
			return 0, fmt.Errorf("update provider: status=%d err=%v", status, err)
		}
		// The admin endpoint atomically owns allowed_tables + table_pages.
		if isBQ {
			if err := writeBQProviderBoundary(c, existing.ID, allowedJSON, cfg.tablePageContents); err != nil {
				return 0, err
			}
		}
		return existing.ID, nil
	}

	// Create — use writeForOrg so the org header is set (POST requires it for
	// tenant assignment; the old c.post call omitted this and caused 400s).
	fmt.Printf("provision: creating provider %q\n", cfg.Name)
	respBody, status, err := c.writeForOrg("POST", "/custom-ai-providers", payloadBytes, orgID)
	if err != nil || status >= 300 {
		return 0, fmt.Errorf("create provider: status=%d err=%v body=%s", status, err, provisionSummarize(respBody))
	}
	if c.dryRun {
		return 0, nil // dry-run returns {} — no real ID, nothing to pin
	}
	var created providerItem
	if err := json.Unmarshal(respBody, &created); err != nil || created.ID == 0 {
		return 0, fmt.Errorf("parse create response: %w body=%s", err, provisionSummarize(respBody))
	}
	if isBQ {
		if err := writeBQProviderBoundary(c, created.ID, allowedJSON, cfg.tablePageContents); err != nil {
			return 0, err
		}
	}
	fmt.Printf("provision: created provider %q id=%d\n", cfg.Name, created.ID)
	return created.ID, nil
}

// providerMapFieldChanges describes how the configured map fields differ from
// the stored provider. An absent (nil) field never changes; an explicit empty
// map against a stored non-empty one is reported as a clear.
func providerMapFieldChanges(cfg providerConfig, existing providerItem) []string {
	var changes []string
	for _, f := range []struct {
		name     string
		want     map[string]string
		stored   string
		keysOnly bool // header values can carry credentials: never print them
	}{
		{"response_mappings", cfg.ResponseMappings, existing.ResponseMappings, false},
		{"request_headers", cfg.RequestHeaders, existing.RequestHeaders, true},
	} {
		if f.want == nil {
			continue
		}
		var have map[string]string
		if f.stored != "" {
			if err := json.Unmarshal([]byte(f.stored), &have); err != nil {
				// Name the shape, never the value: request_headers can carry credentials.
				shape := "not a JSON object"
				var obj map[string]json.RawMessage
				switch {
				case !json.Valid([]byte(f.stored)):
					shape = "not valid JSON"
				case json.Unmarshal([]byte(f.stored), &obj) == nil:
					shape = "not a string map"
				}
				changes = append(changes, fmt.Sprintf("%s: stored value is %s, will be replaced", f.name, shape))
				continue
			}
		}
		if maps.Equal(f.want, have) {
			continue
		}
		render := func(m map[string]string) string {
			if f.keysOnly {
				return "keys " + fmt.Sprint(slices.Sorted(maps.Keys(m)))
			}
			b, _ := json.Marshal(m)
			return string(b)
		}
		if len(f.want) == 0 {
			changes = append(changes, fmt.Sprintf("%s: %s -> {} (clears the stored value)", f.name, render(have)))
		} else {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", f.name, render(have), render(f.want)))
		}
	}
	return changes
}

func validateProviderBoundary(cfg providerConfig) error {
	if strings.EqualFold(cfg.ProviderType, "bigquery") && cfg.AllowedTables == nil {
		return fmt.Errorf("bigquery provider %q must declare allowed_tables (use allowed_tables: [] to clear it intentionally)", cfg.Name)
	}
	return nil
}

// applyProviders upserts every provider declared under dir (the single root
// provider.yaml, if present, plus every *.yaml under providers/). It returns
// the primary provider's ID (root provider.yaml — dashboards default to this
// one, unchanged behavior) and a slug→ID lookup covering ALL upserted
// providers, root included, so a dashboard can opt into a non-primary
// provider via its own "provider" field (see provisionDashboardDef.Provider).
func applyProviders(c *provisionClient, dir, repoRoot string, orgID uint) (uint, map[string]uint, error) {
	var primaryID uint
	bySlug := make(map[string]uint)

	// Single provider at root
	if pf := filepath.Join(dir, "provider.yaml"); fileExists(pf) {
		var cfg providerConfig
		mustReadYAML(pf, &cfg)
		if err := resolveTablePages(repoRoot, &cfg, c.dryRun); err != nil {
			return 0, nil, fmt.Errorf("provider: %w", err)
		}
		if cfg.SkipUpsert {
			fmt.Printf("provision: skipping provider %q (skip_upsert=true, id=%d)\n", cfg.Name, cfg.ID)
			primaryID = uint(cfg.ID)
		} else {
			id, err := upsertProvider(c, orgID, cfg)
			if err != nil {
				return 0, nil, fmt.Errorf("provider: %w", err)
			}
			if !c.dryRun {
				if err := pinProviderID(pf, cfg.ID, int(id)); err != nil {
					return 0, nil, fmt.Errorf("pin provider id: %w", err)
				}
			}
			primaryID = uint(id)
		}
		if cfg.Slug != "" {
			bySlug[cfg.Slug] = primaryID
		}
	}

	// Multi-provider directory
	pd := filepath.Join(dir, "providers")
	if fileExists(pd) {
		entries, err := os.ReadDir(pd)
		if err != nil {
			return 0, nil, fmt.Errorf("providers/: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			pf := filepath.Join(pd, e.Name())
			var cfg providerConfig
			mustReadYAML(pf, &cfg)
			if err := resolveTablePages(repoRoot, &cfg, c.dryRun); err != nil {
				return 0, nil, fmt.Errorf("provider %s: %w", e.Name(), err)
			}
			if cfg.SkipUpsert {
				fmt.Printf("provision: skipping provider %q (skip_upsert=true)\n", cfg.Name)
				if cfg.Slug != "" {
					bySlug[cfg.Slug] = uint(cfg.ID)
				}
				continue
			}
			id, err := upsertProvider(c, orgID, cfg)
			if err != nil {
				return 0, nil, fmt.Errorf("provider %s: %w", e.Name(), err)
			}
			if !c.dryRun {
				if err := pinProviderID(pf, cfg.ID, int(id)); err != nil {
					return 0, nil, fmt.Errorf("pin provider id %s: %w", e.Name(), err)
				}
			}
			if cfg.Slug != "" {
				bySlug[cfg.Slug] = uint(id)
			}
		}
	}

	return primaryID, bySlug, nil
}
