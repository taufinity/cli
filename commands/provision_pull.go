package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ─── Cobra subcommands ───────────────────────────────────────────────────────

var provisionPullDashboardsCmd = &cobra.Command{
	Use:   "dashboards",
	Short: "Snapshot Studio dashboard definitions to local JSON spec files",
	Long: `Fetches all dashboard definitions for the target org and writes them
as JSON spec files into <dir>/dashboards/. Only slugs that already have a
local file are refreshed (pull captures Studio-side edits back into tracked
specs; untracked dashboards are reported and skipped).

Use before editing dashboards to avoid clobbering UI changes.`,
	RunE: runProvisionPullDashboards,
}

var provisionPullPlaybooksCmd = &cobra.Command{
	Use:   "playbooks",
	Short: "Snapshot Studio playbooks to local YAML spec files",
	Long: `Fetches all playbooks for the target org and writes one <slug>.yaml per
playbook into <dir>/playbooks/. Credential IDs are reversed to portable
credential_ref names so the pulled YAML round-trips cleanly through apply.`,
	RunE: runProvisionPullPlaybooks,
}

var provisionPullPresentationTemplatesCmd = &cobra.Command{
	Use:   "presentation-templates",
	Short: "Snapshot Studio presentation templates to local HTML files",
	Long: `Fetches every presentation template for the target org and writes one
<slug>.html per template into <dir>/presentation-templates/. Each file starts
with a "taufinity-provision" HTML comment header (name, uuid, is_default,
branch) followed by the raw compiled_template HTML unchanged. Pulls
unconditionally — unlike dashboards-pull, there's no "only refresh tracked"
gate, since the common case is bootstrapping a source of truth from scratch.`,
	RunE: runProvisionPullPresentationTemplates,
}

func init() {
	provisionPullCmd.AddCommand(provisionPullDashboardsCmd)
	provisionPullCmd.AddCommand(provisionPullPlaybooksCmd)
	provisionPullCmd.AddCommand(provisionPullPresentationTemplatesCmd)
}

func runProvisionPullDashboards(cmd *cobra.Command, args []string) error {
	key, err := resolveProvisionAPIKey()
	if err != nil {
		return err
	}
	c := newProvisionClient(GetAPIURL(), key, IsDryRun())
	orgID, err := resolveProvisionOrgID(c, provisionOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", provisionOrgSlug, err)
	}
	dashDir := filepath.Join(provisionDir, "dashboards")
	return pullProvisionDashboards(c, orgID, dashDir, IsDryRun())
}

func runProvisionPullPlaybooks(cmd *cobra.Command, args []string) error {
	key, err := resolveProvisionAPIKey()
	if err != nil {
		return err
	}
	c := newProvisionClient(GetAPIURL(), key, IsDryRun())
	orgID, err := resolveProvisionOrgID(c, provisionOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", provisionOrgSlug, err)
	}
	outDir := filepath.Join(provisionDir, "playbooks")
	if !IsDryRun() {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", outDir, err)
		}
	}
	return pullProvisionPlaybooks(c, orgID, outDir, IsDryRun())
}

func runProvisionPullPresentationTemplates(cmd *cobra.Command, args []string) error {
	key, err := resolveProvisionAPIKey()
	if err != nil {
		return err
	}
	c := newProvisionClient(GetAPIURL(), key, IsDryRun())
	orgID, err := resolveProvisionOrgID(c, provisionOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", provisionOrgSlug, err)
	}
	outDir := filepath.Join(provisionDir, "presentation-templates")
	return pullProvisionPresentationTemplates(c, orgID, outDir, IsDryRun())
}

// ─── Playbook pull ───────────────────────────────────────────────────────────

// playbookDetailRemote is the GET /api/playbooks/{id} shape (provisionable
// fields only — id/timestamps/run state are ignored). It is also the "remote"
// side of the apply-time drift diff, so every field provision can write must be
// readable here: a field we send but cannot read back is a field we cannot tell
// has drifted.
type playbookDetailRemote struct {
	Name             string  `json:"name"`
	Slug             string  `json:"slug"`
	Description      string  `json:"description"`
	TriggerType      string  `json:"trigger_type"`
	Schedule         *string `json:"schedule"`
	ScheduleTimezone string  `json:"schedule_timezone"`
	SchedulePaused   bool    `json:"schedule_paused"`
	OutputKey        string  `json:"output_key"`
	Enabled          bool    `json:"enabled"`
	AgentTriggerable bool    `json:"agent_triggerable"`
	AgentInputSchema string  `json:"agent_input_schema"`
}

func pullProvisionPlaybooks(c *provisionClient, orgID uint, outDir string, dryRun bool) error {
	body, status, err := c.getForOrg("/playbooks/", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list playbooks", status, body, err)
	}
	var list []provisionPlaybookListItem
	if err := unmarshalListEnvelope(body, &list); err != nil {
		return fmt.Errorf("parse playbooks: %w", err)
	}
	if len(list) == 0 {
		fmt.Println("provision: pull playbooks: no playbooks found")
		return nil
	}

	// Build credential id→name map for reversing credential_id → credential_ref.
	credByID := map[uint]string{}
	if byName, err := listCredentialsByName(c, orgID); err == nil {
		for name, id := range byName {
			credByID[id] = name
		}
	} else {
		c.Warn("playbook-pull: could not list credentials (%v) — credential_id not reversed", err)
	}

	var written, failed int
	seenPath := make(map[string]string)
	for _, p := range list {
		cfg, err := fetchProvisionPlaybookConfig(c, orgID, p, credByID)
		if err != nil {
			c.Warn("playbook-pull: %q (id=%d): %v", p.Name, p.ID, err)
			failed++
			continue
		}
		fileSlug := cfg.Slug
		if fileSlug == "" {
			fileSlug = provisionSlugify(cfg.Name)
		}
		path := filepath.Join(outDir, fileSlug+".yaml")
		if first, dup := seenPath[path]; dup {
			c.Warn("playbook-pull: %q and %q both map to %s — skipping duplicate", first, cfg.Name, path)
			failed++
			continue
		}
		seenPath[path] = cfg.Name
		if dryRun {
			fmt.Printf("  [dry-run] would write %s (%d steps)\n", path, len(cfg.Steps))
			written++
			continue
		}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			c.Warn("playbook-pull: marshal %q: %v", p.Name, err)
			failed++
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			c.Warn("playbook-pull: write %s: %v", path, err)
			failed++
			continue
		}
		fmt.Printf("  wrote %s (%d steps)\n", path, len(cfg.Steps))
		written++
	}

	fmt.Printf("provision: playbooks pull: %d written, %d failed\n", written, failed)
	if failed > 0 {
		return fmt.Errorf("playbook-pull: %d failure(s)", failed)
	}
	return nil
}

func fetchProvisionPlaybookConfig(c *provisionClient, orgID uint, item provisionPlaybookListItem, credByID map[uint]string) (playbookConfig, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d", item.ID), orgID)
	if err != nil || status != 200 {
		return playbookConfig{}, provisionAPIErr(fmt.Sprintf("get playbook %d", item.ID), status, body, err)
	}
	var d playbookDetailRemote
	if err := json.Unmarshal(body, &d); err != nil {
		return playbookConfig{}, fmt.Errorf("parse playbook detail: %w", err)
	}

	cfg := playbookConfig{
		Name:             d.Name,
		Slug:             d.Slug,
		Description:      d.Description,
		TriggerType:      d.TriggerType,
		Schedule:         d.Schedule,
		ScheduleTimezone: d.ScheduleTimezone,
		OutputKey:        d.OutputKey,
		AgentInputSchema: d.AgentInputSchema,
	}
	// Only emit schedule_paused for scheduled playbooks: on an unscheduled one it
	// is meaningless noise, and apply would POST the pause endpoint for a playbook
	// that has no cron.
	if d.Schedule != nil && *d.Schedule != "" {
		paused := d.SchedulePaused
		cfg.SchedulePaused = &paused
	}
	// Bools are pointers in the YAML shape so false is emittable; always set them
	// explicitly so the pulled YAML is unambiguous.
	enabled := d.Enabled
	agentTrig := d.AgentTriggerable
	cfg.Enabled = &enabled
	cfg.AgentTriggerable = &agentTrig

	sbody, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d/steps", item.ID), orgID)
	if err != nil || status != 200 {
		return playbookConfig{}, provisionAPIErr(fmt.Sprintf("get playbook steps %d", item.ID), status, sbody, err)
	}
	var remoteSteps []provisionPlaybookStepRemote
	if err := unmarshalListEnvelope(sbody, &remoteSteps); err != nil {
		return playbookConfig{}, fmt.Errorf("parse steps: %w", err)
	}
	sort.SliceStable(remoteSteps, func(i, j int) bool {
		return remoteSteps[i].Position < remoteSteps[j].Position
	})
	for _, rs := range remoteSteps {
		stepCfg, err := stepConfigFromJSON(rs.Config, credByID)
		if err != nil {
			return playbookConfig{}, fmt.Errorf("step %q config: %w", rs.Name, err)
		}
		e := rs.Enabled
		cfg.Steps = append(cfg.Steps, playbookStep{
			Name:     rs.Name,
			StepType: rs.StepType,
			// Position intentionally omitted — derived from array index on apply.
			Config:      stepCfg,
			OutputKey:   rs.OutputKey,
			Enabled:     &e,
			ErrorPolicy: rs.ErrorPolicy,
		})
	}
	return cfg, nil
}

// stepConfigFromJSON reverses stepConfigToJSON: the wire form is a JSON string;
// decode it to a generic value for YAML emission, and reverse
// credential_id → credential_ref using the id→name map.
func stepConfigFromJSON(jsonStr string, credByID map[uint]string) (interface{}, error) {
	trimmed := strings.TrimSpace(jsonStr)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return nil, nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return nil, fmt.Errorf("unmarshal config json: %w", err)
	}
	return reverseProvisionCredentialID(v, credByID), nil
}

func reverseProvisionCredentialID(v interface{}, credByID map[uint]string) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, vv := range x {
			out[k] = reverseProvisionCredentialID(vv, credByID)
		}
		if idRaw, ok := out["credential_id"]; ok {
			if id, ok := provisionCredIDToUint(idRaw); ok {
				if name, found := credByID[id]; found {
					delete(out, "credential_id")
					out["credential_ref"] = name
				}
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, vv := range x {
			out[i] = reverseProvisionCredentialID(vv, credByID)
		}
		return out
	default:
		return v
	}
}

func provisionCredIDToUint(v interface{}) (uint, bool) {
	switch n := v.(type) {
	case float64:
		if n >= 0 && n == float64(uint(n)) {
			return uint(n), true
		}
	case int:
		if n >= 0 {
			return uint(n), true
		}
	case uint:
		return n, true
	}
	return 0, false
}

func provisionSlugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
