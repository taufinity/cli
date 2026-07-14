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

func init() {
	provisionPullCmd.AddCommand(provisionPullDashboardsCmd)
	provisionPullCmd.AddCommand(provisionPullPlaybooksCmd)
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

// ─── Dashboard pull ──────────────────────────────────────────────────────────

// provisionDashboardDetail is the rich wire shape returned by
// GET /api/admin/dashboard-definitions/{id}.
// Uses a separate type (not dashboardDef) to avoid collision with the minimal
// dashboardDef already defined in dashboards.go for sync operations.
type provisionDashboardDetail struct {
	ID                 uint            `json:"id"`
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	SourceView         string          `json:"source_view"`
	Columns            json.RawMessage `json:"columns"`
	Filters            json.RawMessage `json:"filters,omitempty"`
	DefaultChart       string          `json:"default_chart,omitempty"`
	DefaultSort        json.RawMessage `json:"default_sort,omitempty"`
	HiddenFromOverview bool            `json:"hidden_from_overview,omitempty"`
	ExportEnabled      bool            `json:"export_enabled,omitempty"`
	MaxRows            int             `json:"max_rows,omitempty"`
	Position           int             `json:"position,omitempty"`
	Layout             json.RawMessage `json:"layout,omitempty"`
	StaticFilters      json.RawMessage `json:"source_filter,omitempty"`
	ClientGroupFilter  json.RawMessage `json:"client_group_filter,omitempty"`
	Breadcrumb         string          `json:"breadcrumb,omitempty"`
}

type provisionDashboardFileShape struct {
	Slug               string          `json:"slug"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	SourceView         string          `json:"source_view"`
	Columns            json.RawMessage `json:"columns"`
	Filters            json.RawMessage `json:"filters,omitempty"`
	DefaultChart       string          `json:"default_chart,omitempty"`
	DefaultSort        json.RawMessage `json:"default_sort,omitempty"`
	HiddenFromOverview bool            `json:"hidden_from_overview,omitempty"`
	ExportEnabled      bool            `json:"export_enabled,omitempty"`
	MaxRows            int             `json:"max_rows,omitempty"`
	Position           int             `json:"position,omitempty"`
	Layout             json.RawMessage `json:"layout,omitempty"`
	StaticFilters      json.RawMessage `json:"source_filter,omitempty"`
	ClientGroupFilter  json.RawMessage `json:"client_group_filter,omitempty"`
	Breadcrumb         string          `json:"breadcrumb,omitempty"`
}

func (d *provisionDashboardDetail) toFileShape() provisionDashboardFileShape {
	return provisionDashboardFileShape{
		Slug:               d.Slug,
		Name:               d.Name,
		Description:        d.Description,
		SourceView:         d.SourceView,
		Columns:            d.Columns,
		Filters:            d.Filters,
		DefaultChart:       d.DefaultChart,
		DefaultSort:        d.DefaultSort,
		HiddenFromOverview: d.HiddenFromOverview,
		ExportEnabled:      d.ExportEnabled,
		MaxRows:            d.MaxRows,
		Position:           d.Position,
		Layout:             d.Layout,
		StaticFilters:      d.StaticFilters,
		ClientGroupFilter:  d.ClientGroupFilter,
		Breadcrumb:         d.Breadcrumb,
	}
}

func getProvisionDashboardDetail(c *provisionClient, orgID, id uint) (*provisionDashboardDetail, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/admin/dashboard-definitions/%d", id), orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr(fmt.Sprintf("get dashboard detail %d", id), status, body, err)
	}
	var d provisionDashboardDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("parse dashboard detail: %w", err)
	}
	return &d, nil
}

type provisionDashboardListItem struct {
	ID   uint   `json:"id"`
	Slug string `json:"slug"`
}

func tryUnmarshalProvisionDashboardList(body []byte, out *[]provisionDashboardListItem) error {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		return json.Unmarshal(body, out)
	}
	var wrapped struct {
		Definitions []provisionDashboardListItem `json:"definitions"`
		Data        []provisionDashboardListItem `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return err
	}
	if len(wrapped.Definitions) > 0 {
		*out = wrapped.Definitions
	} else {
		*out = wrapped.Data
	}
	return nil
}

func localProvisionDashboardFiles(dir string) (map[string]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		if strings.HasPrefix(filepath.Base(m), "_") {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", m, err)
		}
		var d struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		if d.Slug != "" {
			out[d.Slug] = m
		}
	}
	return out, nil
}

func writeProvisionDashboardFile(path string, d provisionDashboardFileShape) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func pullProvisionDashboards(c *provisionClient, orgID uint, dir string, dryRun bool) error {
	body, status, err := c.getForOrg("/admin/dashboard-definitions", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list dashboards", status, body, err)
	}
	var existing []provisionDashboardListItem
	if err := tryUnmarshalProvisionDashboardList(body, &existing); err != nil {
		return fmt.Errorf("parse dashboards: %w", err)
	}

	localBySlug, err := localProvisionDashboardFiles(dir)
	if err != nil {
		return err
	}

	pulled, skipped := 0, 0
	for _, d := range existing {
		path, tracked := localBySlug[d.Slug]
		if !tracked {
			fmt.Printf("SKIP   %s (in Studio, no local file)\n", d.Slug)
			skipped++
			continue
		}
		detail, err := getProvisionDashboardDetail(c, orgID, d.ID)
		if err != nil {
			return fmt.Errorf("get detail %q: %w", d.Slug, err)
		}
		if dryRun {
			fmt.Printf("WOULD PULL %s id=%d → %s\n", d.Slug, d.ID, path)
			pulled++
			continue
		}
		if err := writeProvisionDashboardFile(path, detail.toFileShape()); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("PULL   %s id=%d → %s\n", d.Slug, d.ID, path)
		pulled++
	}
	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	fmt.Printf("provision: dashboards pull: %s=%d skipped=%d\n", verb, pulled, skipped)
	return nil
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
