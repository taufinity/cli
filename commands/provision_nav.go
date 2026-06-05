package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// navYAML is the top-level envelope for studio/nav.yaml.
type navYAML struct {
	Nav navBlock `yaml:"nav"`
}

// navBlock holds all nav-profile settings for an org.
type navBlock struct {
	// RoleDefaults maps role names (owner, admin, member, viewer, client_group)
	// to profile slugs (full_access, content_editor, …, or any custom profile).
	RoleDefaults map[string]string `yaml:"role_defaults"`
	// CustomProfiles defines org-specific profile slugs and their item lists.
	CustomProfiles map[string]customProfileYAML `yaml:"custom_profiles"`
	// ClientGroups overrides nav per client group slug (rarely needed — prefer
	// the nav_config field in the per-group YAML under access/client-groups/).
	ClientGroups map[string]navConfigYAML `yaml:"client_groups"`
}

// customProfileYAML lists the nav slugs visible to that profile.
type customProfileYAML struct {
	Items []string `yaml:"items"`
}

// parseNavYAML parses the raw bytes of a nav.yaml file and returns the navBlock.
func parseNavYAML(data []byte) (*navBlock, error) {
	var top navYAML
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse nav.yaml: %w", err)
	}
	return &top.Nav, nil
}

// provisionNavSettings writes all nav settings from a navBlock to the API.
// For each role default it calls POST /admin/organizations/{id}/settings with
// key=nav.role.{role} and value={profile}. For custom_profiles it serialises
// the entire map as JSON and stores it under nav.custom_profiles.
func provisionNavSettings(c *provisionClient, orgID uint, nav *navBlock, dryRun bool) error {
	// Role defaults — one KV write per role
	for role, profile := range nav.RoleDefaults {
		key := "nav.role." + role
		if dryRun {
			fmt.Printf("provision(dry-run): org %d nav setting %s = %q\n", orgID, key, profile)
			continue
		}
		payload, _ := json.Marshal(map[string]string{"key": key, "value": profile})
		endpoint := fmt.Sprintf("/admin/organizations/%d/settings", orgID)
		_, status, err := c.write("POST", endpoint, payload)
		if err != nil || status >= 300 {
			return fmt.Errorf("set %s: status=%d err=%v", key, status, err)
		}
		fmt.Printf("provision: org %d nav.role.%s = %q\n", orgID, role, profile)
	}

	// Custom profiles — serialise whole map as JSON into a single KV key
	if len(nav.CustomProfiles) > 0 {
		profiles := make(map[string][]string, len(nav.CustomProfiles))
		for slug, p := range nav.CustomProfiles {
			profiles[slug] = p.Items
		}
		profilesJSON, err := json.Marshal(profiles)
		if err != nil {
			return fmt.Errorf("marshal custom_profiles: %w", err)
		}
		if dryRun {
			fmt.Printf("provision(dry-run): org %d nav.custom_profiles = %s\n", orgID, profilesJSON)
		} else {
			payload, _ := json.Marshal(map[string]string{"key": "nav.custom_profiles", "value": string(profilesJSON)})
			endpoint := fmt.Sprintf("/admin/organizations/%d/settings", orgID)
			_, status, err := c.write("POST", endpoint, payload)
			if err != nil || status >= 300 {
				return fmt.Errorf("set nav.custom_profiles: status=%d err=%v", status, err)
			}
			fmt.Printf("provision: org %d nav.custom_profiles updated (%d profiles)\n", orgID, len(profiles))
		}
	}
	return nil
}

func applyNav(c *provisionClient, dir string, orgID uint) error {
	nf := filepath.Join(dir, "nav.yaml")
	if !fileExists(nf) {
		return nil
	}
	data, err := os.ReadFile(nf)
	if err != nil {
		return fmt.Errorf("read nav.yaml: %w", err)
	}
	nav, err := parseNavYAML(data)
	if err != nil {
		return err
	}
	return provisionNavSettings(c, orgID, nav, c.dryRun)
}
