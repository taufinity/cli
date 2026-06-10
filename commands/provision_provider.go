package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	Slug           string   `yaml:"slug,omitempty"`
	Description    string   `yaml:"description"`
	ProviderType   string   `yaml:"provider_type"`
	Category       string   `yaml:"category"`
	EndpointURL    string   `yaml:"endpoint_url"`
	HTTPMethod     string   `yaml:"http_method"`
	AllowedTables  []string `yaml:"allowed_tables"`
	MaxBytesBilled int64    `yaml:"max_bytes_billed"`
	Enabled        bool     `yaml:"enabled"`

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

type providerItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// upsertProvider creates or updates a provider for the given org.
// When cfg.ID > 0 it matches by ID (safe rename support); otherwise falls back
// to case-insensitive name match.
// Returns the live provider ID (existing or newly created) so the caller can
// write it back to the YAML file as a pinned id.
func upsertProvider(c *provisionClient, orgID uint, cfg providerConfig) (int, error) {
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
		allowedJSON, err = json.Marshal(cfg.AllowedTables)
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
	if len(cfg.ResponseMappings) > 0 {
		mappingsJSON, err := json.Marshal(cfg.ResponseMappings)
		if err != nil {
			return 0, fmt.Errorf("marshal response_mappings: %w", err)
		}
		payload["response_mappings"] = string(mappingsJSON)
	}
	if cfg.ResponseJSONPath != "" {
		payload["response_json_path"] = cfg.ResponseJSONPath
	}
	if len(cfg.RequestHeaders) > 0 {
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

	// Match existing provider: by slug when set (env-independent), else by ID
	// when pinned, else by case-insensitive name.
	for _, existing := range items {
		var matched bool
		switch {
		case cfg.Slug != "":
			matched = existing.Slug != "" && existing.Slug == cfg.Slug
		case cfg.ID > 0:
			matched = existing.ID == cfg.ID
		default:
			matched = strings.EqualFold(existing.Name, cfg.Name)
		}
		if !matched {
			continue
		}
		if cfg.ID > 0 && !strings.EqualFold(existing.Name, cfg.Name) {
			fmt.Printf("provision: provider id=%d name changed %q → %q\n", existing.ID, existing.Name, cfg.Name)
		}
		fmt.Printf("provision: updating provider %q (id=%d)\n", cfg.Name, existing.ID)
		_, status, err = c.put(fmt.Sprintf("/custom-ai-providers/%d", existing.ID), payloadBytes)
		if err != nil || status >= 300 {
			return 0, fmt.Errorf("update provider: status=%d err=%v", status, err)
		}
		// PUT /custom-ai-providers/{id} doesn't process allowed_tables — BQ providers
		// need a second call to the admin endpoint which owns that field.
		if isBQ && len(cfg.AllowedTables) > 0 {
			bqPayload, _ := json.Marshal(map[string]interface{}{
				"allowed_tables": string(allowedJSON),
			})
			_, status, err = c.put(fmt.Sprintf("/admin/bq-providers/%d", existing.ID), bqPayload)
			if err != nil || status >= 300 {
				return 0, fmt.Errorf("update BQ allowed_tables: status=%d err=%v", status, err)
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
	fmt.Printf("provision: created provider %q id=%d\n", cfg.Name, created.ID)
	return created.ID, nil
}

// pinProviderID writes `id: <liveID>` into the YAML file when the provider was
// just created (cfgID == 0) or when the pinned id in the file is stale.
// It preserves all comments and formatting by doing a targeted line edit on the
// raw bytes rather than a full YAML round-trip.
func pinProviderID(path string, cfgID, liveID int) error {
	if liveID == 0 {
		return nil // nothing to pin
	}
	if cfgID == liveID {
		return nil // already pinned and correct
	}
	// cfgID != 0 and cfgID != liveID means the YAML had an ID pinned but the
	// server returned a different one (stale pin, or name/slug matched a different
	// record). Log a warning before overwriting so operators can catch env mixups.
	if cfgID != 0 {
		fmt.Printf("  WARN: provider id in YAML (%d) differs from server (%d) — updating pin in %s\n", cfgID, liveID, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Replace existing top-level `id: N` line, or insert one before `name:`.
	idLine := regexp.MustCompile(`(?m)^id:\s+\d+`)
	newIDLine := "id: " + strconv.Itoa(liveID)
	if idLine.Match(raw) {
		raw = idLine.ReplaceAll(raw, []byte(newIDLine))
	} else {
		// Insert before the `name:` line.
		nameLine := regexp.MustCompile(`(?m)^name:`)
		if loc := nameLine.FindIndex(raw); loc != nil {
			insert := []byte(newIDLine + "\n")
			raw = append(raw[:loc[0]], append(insert, raw[loc[0]:]...)...)
		} else {
			// Fallback: prepend.
			raw = append([]byte(newIDLine+"\n"), raw...)
		}
	}

	if err := os.WriteFile(path, raw, 0644); err != nil {
		return err
	}
	fmt.Printf("provision: pinned provider id=%d in %s\n", liveID, path)
	return nil
}

func applyProviders(c *provisionClient, dir string, orgID uint) (uint, error) {
	var primaryID uint

	// Single provider at root
	if pf := filepath.Join(dir, "provider.yaml"); fileExists(pf) {
		var cfg providerConfig
		mustReadYAML(pf, &cfg)
		if cfg.SkipUpsert {
			fmt.Printf("provision: skipping provider %q (skip_upsert=true, id=%d)\n", cfg.Name, cfg.ID)
			primaryID = uint(cfg.ID)
		} else {
			id, err := upsertProvider(c, orgID, cfg)
			if err != nil {
				return 0, fmt.Errorf("provider: %w", err)
			}
			if !c.dryRun {
				if err := pinProviderID(pf, cfg.ID, int(id)); err != nil {
					return 0, fmt.Errorf("pin provider id: %w", err)
				}
			}
			primaryID = uint(id)
		}
	}

	// Multi-provider directory
	pd := filepath.Join(dir, "providers")
	if fileExists(pd) {
		entries, err := os.ReadDir(pd)
		if err != nil {
			return 0, fmt.Errorf("providers/: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			pf := filepath.Join(pd, e.Name())
			var cfg providerConfig
			mustReadYAML(pf, &cfg)
			if cfg.SkipUpsert {
				fmt.Printf("provision: skipping provider %q (skip_upsert=true)\n", cfg.Name)
				continue
			}
			id, err := upsertProvider(c, orgID, cfg)
			if err != nil {
				return 0, fmt.Errorf("provider %s: %w", e.Name(), err)
			}
			if !c.dryRun {
				if err := pinProviderID(pf, cfg.ID, int(id)); err != nil {
					return 0, fmt.Errorf("pin provider id %s: %w", e.Name(), err)
				}
			}
		}
	}

	return primaryID, nil
}
