package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// studioAPIAllowlistEntry mirrors ai-site-gen's
// pkg/database.StudioAPIAllowlistEntry — kept as a local copy (this repo
// doesn't import ai-site-gen) rather than a shared type, same reasoning as
// every other provisioner's YAML config struct in this package.
type studioAPIAllowlistEntry struct {
	Method                 string `yaml:"method" json:"method"`
	PathPattern            string `yaml:"path_pattern" json:"path_pattern"`
	BlockIfTargetPublished bool   `yaml:"block_if_target_published,omitempty" json:"block_if_target_published,omitempty"`
}

type agentToolsConfig struct {
	AgentToolPolicy    []string                  `yaml:"agent_tool_policy"`
	StudioAPIAllowlist []studioAPIAllowlistEntry `yaml:"studio_api_allowlist"`
}

// applyAgentTools sets an org's AgentToolPolicy grants and
// StudioAPIAllowlist entries via PATCH /organizations/{id} — the two fields
// that together govern what an autonomous agent run may do for this org.
// Security-sensitive: notify_human reaches a real human inbox,
// call_studio_api can edit/publish live content — treat changes here with
// the same care as any other access grant, and review the diff in
// version control before applying, same as every other provisioned config.
//
// Absent from the YAML entirely means "leave unchanged" for each field
// independently (matches the server's own nil-vs-empty-slice convention,
// see ai-site-gen's api/handlers/organizations_update.go) — an
// agent-tools.yaml that only sets agent_tool_policy does not touch
// studio_api_allowlist, and vice versa. To explicitly clear a field, set it
// to an empty list (`agent_tool_policy: []`), not by omitting the key.
func applyAgentTools(c *provisionClient, dir string, orgID uint) error {
	af := filepath.Join(dir, "agent-tools.yaml")
	if !fileExists(af) {
		return nil
	}
	var cfg agentToolsConfig
	mustReadYAML(af, &cfg)

	body := map[string]any{}
	if cfg.AgentToolPolicy != nil {
		body["agent_tool_policy"] = cfg.AgentToolPolicy
	}
	if cfg.StudioAPIAllowlist != nil {
		body["studio_api_allowlist"] = cfg.StudioAPIAllowlist
	}
	if len(body) == 0 {
		return nil
	}

	if c.dryRun {
		fmt.Printf("  [dry-run] agent-tools: agent_tool_policy=%v studio_api_allowlist=%d entries\n",
			cfg.AgentToolPolicy, len(cfg.StudioAPIAllowlist))
		return nil
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal agent-tools config: %w", err)
	}
	_, status, err := c.patch(fmt.Sprintf("/organizations/%d", orgID), payload)
	if err != nil {
		return fmt.Errorf("apply agent-tools: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("apply agent-tools: unexpected status %d", status)
	}
	fmt.Printf("provision: agent_tool_policy=%v studio_api_allowlist=%d entries\n",
		cfg.AgentToolPolicy, len(cfg.StudioAPIAllowlist))
	return nil
}
