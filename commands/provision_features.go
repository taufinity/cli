package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

type featuresConfig struct {
	Flags map[string]bool `yaml:"flags"`
}

// upsertFeatureFlags sets org-level feature flag overrides via the admin API.
// Each key in cfg.Flags maps to PUT /api/admin/organizations/{orgID}/features/{key}.
// In dry-run mode it prints the intended change without calling the API.
func upsertFeatureFlags(c *provisionClient, orgID uint, cfg featuresConfig) error {
	for key, enabled := range cfg.Flags {
		value := "false"
		if enabled {
			value = "true"
		}
		if c.dryRun {
			fmt.Printf("  [dry-run] feature %s = %s\n", key, value)
			continue
		}
		body, err := json.Marshal(map[string]string{"value": value})
		if err != nil {
			return fmt.Errorf("marshal feature %s: %w", key, err)
		}
		path := fmt.Sprintf("/admin/organizations/%d/features/%s", orgID, key)
		_, status, err := c.put(path, body)
		if err != nil {
			return fmt.Errorf("set feature %s: %w", key, err)
		}
		if status != 200 {
			return fmt.Errorf("set feature %s: unexpected status %d", key, status)
		}
		fmt.Printf("provision: feature %s = %s\n", key, value)
	}
	return nil
}

func applyFeatureFlags(c *provisionClient, dir string, orgID uint) error {
	ff := filepath.Join(dir, "features.yaml")
	if !fileExists(ff) {
		return nil
	}
	var cfg featuresConfig
	mustReadYAML(ff, &cfg)
	return upsertFeatureFlags(c, orgID, cfg)
}
