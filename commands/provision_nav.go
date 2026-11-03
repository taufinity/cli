package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// navYAML is the top-level envelope for studio/nav.yaml.
type navYAML struct {
	Nav navBlock `yaml:"nav"`
}

// navBlock holds all nav-profile settings for an org.
//
// Every field here maps onto a key in the org's "nav" settings namespace,
// which GET /api/me/nav reads back when the frontend builds the sidebar.
// Keep this struct and provisionNavSettings in step: a field declared here
// but not written there is a silent no-op, which is how default_route came
// to be declared in customer nav.yaml files for months without ever
// reaching Studio.
type navBlock struct {
	// DefaultRoute is the path "/" redirects to for this org (App.tsx's
	// RootRedirect). Without it users land on /reports, which is the wrong
	// first screen for an org whose whole product is one playbook.
	DefaultRoute string `yaml:"default_route"`
	// RoleDefaults maps role names (owner, admin, member, viewer, client_group)
	// to profile slugs (full_access, content_editor, …, or any custom profile).
	RoleDefaults map[string]string `yaml:"role_defaults"`
	// CustomProfiles defines org-specific profile slugs and their item lists.
	CustomProfiles map[string]customProfileYAML `yaml:"custom_profiles"`
	// Labels renames nav items for this org: slug → the word the customer uses.
	// The sidebar's own names are product-wide nouns ("Playbooks", "Offertes");
	// a customer-facing org usually wants verbs in its own language.
	Labels map[string]string `yaml:"labels"`
	// Hide removes items from whatever the resolved profile produced. Use it
	// to subtract from a built-in profile instead of copying the whole list
	// into a custom profile just to drop one entry.
	Hide []string `yaml:"hide"`
	// Pin lists slugs the sidebar pins to the top for every user in the org.
	Pin []string `yaml:"pin"`
	// ClientGroups is accepted but NOT provisioned from here — per-group nav
	// lives in the nav_config field of access/client-groups/<slug>.yaml, which
	// provisionClientGroups writes. Declaring it here used to be a silent
	// no-op; provisionNavSettings now warns rather than quietly dropping it.
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

// navSettingKVs flattens a navBlock into the ordered list of org-setting
// key/value pairs to write. Ordering is deterministic (maps are sorted) so a
// dry-run diff is stable between runs and reviewable.
//
// Removal caveat: these are per-key writes, so dropping a label from nav.yaml
// leaves the previously-written nav.label.<slug> key in place. Clearing one
// today means setting it to "" explicitly in the YAML — GET /api/me/nav skips
// empty values. Wiping the namespace wholesale needs PUT /organizations/{id}/nav,
// which does not yet round-trip these keys.
func navSettingKVs(nav *navBlock) ([]navKV, error) {
	var out []navKV

	if nav.DefaultRoute != "" {
		out = append(out, navKV{"nav.default_route", nav.DefaultRoute})
	}

	for _, role := range sortedKeys(nav.RoleDefaults) {
		out = append(out, navKV{"nav.role." + role, nav.RoleDefaults[role]})
	}

	if len(nav.CustomProfiles) > 0 {
		profiles := make(map[string][]string, len(nav.CustomProfiles))
		for slug, p := range nav.CustomProfiles {
			profiles[slug] = p.Items
		}
		profilesJSON, err := json.Marshal(profiles)
		if err != nil {
			return nil, fmt.Errorf("marshal custom_profiles: %w", err)
		}
		out = append(out, navKV{"nav.custom_profiles", string(profilesJSON)})
	}

	for _, slug := range sortedKeys(nav.Labels) {
		out = append(out, navKV{"nav.label." + slug, nav.Labels[slug]})
	}

	// hide/pin are stored as one JSON array each, matching how GetMyNav
	// unmarshals them. An empty list is skipped rather than written as "[]":
	// writing it would be indistinguishable from "hide nothing" anyway.
	if len(nav.Hide) > 0 {
		b, err := json.Marshal(nav.Hide)
		if err != nil {
			return nil, fmt.Errorf("marshal hide: %w", err)
		}
		out = append(out, navKV{"nav.hide", string(b)})
	}
	if len(nav.Pin) > 0 {
		b, err := json.Marshal(nav.Pin)
		if err != nil {
			return nil, fmt.Errorf("marshal pin: %w", err)
		}
		out = append(out, navKV{"nav.pin", string(b)})
	}

	return out, nil
}

// navKV is one org setting to write.
type navKV struct {
	Key   string
	Value string
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// provisionNavSettings writes all nav settings from a navBlock to the API.
// Each key goes to POST /admin/organizations/{id}/settings, which accepts the
// nav.* namespace specifically.
func provisionNavSettings(c *provisionClient, orgID uint, nav *navBlock, dryRun bool) error {
	if len(nav.ClientGroups) > 0 {
		// Loud, because this was silently dropped before: an org that set it
		// here had every reason to believe its client groups were configured.
		fmt.Printf("provision: WARNING nav.yaml client_groups is ignored (%d entries) — "+
			"set nav_config in access/client-groups/<slug>.yaml instead\n", len(nav.ClientGroups))
	}

	kvs, err := navSettingKVs(nav)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("/admin/organizations/%d/settings", orgID)
	for _, kv := range kvs {
		if dryRun {
			fmt.Printf("provision(dry-run): org %d nav setting %s = %q\n", orgID, kv.Key, kv.Value)
			continue
		}
		payload, _ := json.Marshal(map[string]string{"key": kv.Key, "value": kv.Value})
		_, status, err := c.write("POST", endpoint, payload)
		if err != nil || status >= 300 {
			return fmt.Errorf("set %s: status=%d err=%v", kv.Key, status, err)
		}
		fmt.Printf("provision: org %d %s = %q\n", orgID, kv.Key, kv.Value)
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
