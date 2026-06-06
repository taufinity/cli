// provision_playbooks.go — provision support for playbook resources.
//
// Mirrors the dashboards.go pattern: list, match by name, upsert, then
// reconcile child rows (steps). Idempotent: re-running with the same YAML
// produces NOOPs after the first apply.
//
// Versioning: every PUT/POST hits the standard /api/playbooks endpoints,
// which call SaveVersionWithSummary internally. The X-Change-Source:
// provision header is set automatically by the client (see provision_client.go),
// which the handlers honour via middleware.ResolveChangedByType — version
// rows are tagged "provision" instead of "user"/"system".
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// playbookConfig is the YAML shape for a single playbook + its steps.
//
// Schema example (studio/playbook.yaml):
//
//	name: WVS Offerte Builder
//	description: Genereert offertes uit intake + prijslijst
//	trigger_type: manual          # manual | event | schedule
//	output_key: offerte
//	enabled: true
//	agent_triggerable: false
//	steps:
//	  - name: Extract intake
//	    step_type: llm_extract
//	    position: 0
//	    output_key: intake
//	    enabled: true
//	    config:                   # arbitrary JSON, passed through to the step
//	      model: claude-opus-4-7
//	      schema_ref: wvs-intake-v1
//	  - name: Calculate price
//	    step_type: formula
//	    position: 1
//	    output_key: calculatie
//	    config:
//	      intake_key: intake
//	      price_list_tag: prijslijst-leenen
//	      categories:             # formula categories embedded in step config
//	        - name: Aanleg
//	          slug: aanleg
//	          rules:
//	            - { name: gras, template_type: per_m2, formula: "gras_m2 * gras_prijs" }
//
// Notes:
//   - `config` is marshalled to a JSON string before sending — the step
//     handler accepts a string-encoded JSON column on disk.
//   - Steps with the same name are matched and updated in place; new steps
//     are POSTed; steps removed from YAML are DELETEd. Position from YAML
//     is the source of truth for ordering.
type playbookConfig struct {
	Name             string         `yaml:"name"`
	Slug             string         `yaml:"slug,omitempty"` // stable provision key; leading over name when set
	Description      string         `yaml:"description,omitempty"`
	TriggerType      string         `yaml:"trigger_type,omitempty"`
	Schedule         *string        `yaml:"schedule,omitempty"`
	ScheduleTimezone string         `yaml:"schedule_timezone,omitempty"`
	OutputKey        string         `yaml:"output_key,omitempty"`
	Enabled          *bool          `yaml:"enabled,omitempty"`
	AgentTriggerable *bool          `yaml:"agent_triggerable,omitempty"`
	AgentInputSchema string         `yaml:"agent_input_schema,omitempty"`
	Steps            []playbookStep `yaml:"steps,omitempty"`
	// Verification is documentation-only (non-technical handover checklist).
	// Never sent to the Studio API — declared here so the YAML parser doesn't fail.
	Verification interface{} `yaml:"verification,omitempty"`
}

// playbookStep mirrors database.PlaybookStep for the YAML form. `config` is
// arbitrary YAML/JSON (whatever the step type accepts) — provision just
// marshals it to a JSON string before sending.
type playbookStep struct {
	Name      string      `yaml:"name"`
	StepType  string      `yaml:"step_type"`
	Position  int         `yaml:"position,omitempty"` // deprecated: ignored by provision; derived from array index
	Config    interface{} `yaml:"config,omitempty"`
	OutputKey string      `yaml:"output_key,omitempty"`
	Enabled   *bool       `yaml:"enabled,omitempty"`
}

// provisionPlaybookListItem is the minimal shape we need from GET /api/playbooks/.
type provisionPlaybookListItem struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// provisionPlaybookStepRemote is the API shape for an existing step.
type provisionPlaybookStepRemote struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	StepType  string `json:"step_type"`
	Position  int    `json:"position"`
	Config    string `json:"config"`
	OutputKey string `json:"output_key"`
	Enabled   bool   `json:"enabled"`
}

// applyPlaybooks applies all playbook YAML files from dir to the org.
// Supports a single playbook.yaml at the root and/or a playbooks/ subdirectory.
func applyPlaybooks(c *provisionClient, dir string, orgID uint) error {
	// single playbook.yaml at root
	if pf := filepath.Join(dir, "playbook.yaml"); fileExists(pf) {
		var cfg playbookConfig
		mustReadYAML(pf, &cfg)
		if err := upsertPlaybook(c, orgID, cfg, pf); err != nil {
			return fmt.Errorf("playbook: %w", err)
		}
	}
	// multi-playbook directory
	pd := filepath.Join(dir, "playbooks")
	if fileExists(pd) {
		entries, _ := os.ReadDir(pd)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			pf := filepath.Join(pd, e.Name())
			var cfg playbookConfig
			mustReadYAML(pf, &cfg)
			if err := upsertPlaybook(c, orgID, cfg, pf); err != nil {
				return fmt.Errorf("playbook %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// upsertPlaybook creates or updates a playbook + its steps for the given org.
// Match key: slug (when present in YAML) or case-insensitive name (fallback).
// Refuses to apply if more than one match is found (ambiguous).
// yamlPath is used to write the slug back after first CREATE.
func upsertPlaybook(c *provisionClient, orgID uint, cfg playbookConfig, yamlPath string) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("playbook: name is required")
	}

	// Substitute "ui:uploadWidgetSlug":"foo" in the agent_input_schema with the
	// resolved numeric widget id for this env. Keeps the yaml portable across
	// localhost/staging/prod where widget numeric ids differ.
	if cfg.AgentInputSchema != "" && strings.Contains(cfg.AgentInputSchema, "ui:uploadWidgetSlug") {
		substituted, err := substituteWidgetSlugs(c, orgID, cfg.AgentInputSchema)
		if err != nil {
			return fmt.Errorf("playbook %q: widget slug substitution: %w", cfg.Name, err)
		}
		cfg.AgentInputSchema = substituted
	}

	body, status, err := c.getForOrg("/playbooks/", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list playbooks", status, body, err)
	}
	var existing []provisionPlaybookListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return fmt.Errorf("parse playbooks: %w (body=%s)", err, provisionSummarize(body))
	}
	var matches []provisionPlaybookListItem
	for _, p := range existing {
		if cfg.Slug != "" {
			// Slug-leading: authoritative match when YAML has a slug.
			if p.Slug == cfg.Slug {
				matches = append(matches, p)
			}
		} else {
			// Backwards-compat: name match when no slug in YAML.
			if strings.EqualFold(p.Name, cfg.Name) {
				matches = append(matches, p)
			}
		}
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, fmt.Sprintf("id=%d", m.ID))
		}
		return fmt.Errorf("playbook %q: %d matches in org %d (%s) — ambiguous, refusing to apply",
			cfg.Name, len(matches), orgID, strings.Join(names, ", "))
	}

	var pbID uint
	if len(matches) == 1 {
		pbID = matches[0].ID
		fmt.Printf("UPDATE playbook %q id=%d\n", cfg.Name, pbID)
		payload, _ := json.Marshal(playbookUpdatePayload(cfg))
		respBody, status, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d", pbID), payload, orgID)
		if err != nil || status >= 300 {
			return provisionAPIErr(fmt.Sprintf("update playbook %q", cfg.Name), status, respBody, err)
		}
	} else {
		fmt.Printf("CREATE playbook %q\n", cfg.Name)
		payload, _ := json.Marshal(playbookCreatePayload(cfg))
		respBody, status, err := c.writeForOrg("POST", "/playbooks/", payload, orgID)
		if err != nil || status >= 300 {
			return provisionAPIErr(fmt.Sprintf("create playbook %q", cfg.Name), status, respBody, err)
		}
		var created struct {
			ID               uint   `json:"id"`
			Slug             string `json:"slug"`
			AgentInputSchema string `json:"agent_input_schema"`
		}
		if err := json.Unmarshal(respBody, &created); err != nil || created.ID == 0 {
			// Dry-run path returns "{}" — accept zero ID and skip step sync.
			if c.dryRun {
				fmt.Printf("[dry-run] skipping step sync for playbook %q (no ID returned)\n", cfg.Name)
				return nil
			}
			return fmt.Errorf("create playbook %q: response missing id (body=%s)", cfg.Name, provisionSummarize(respBody))
		}
		pbID = created.ID
		if created.Slug != "" {
			if err := pinSlug(yamlPath, cfg.Slug, created.Slug); err != nil {
				fmt.Printf("  WARN: could not pin slug for playbook %q: %v\n", cfg.Name, err)
			}
		}

		// Verify-after-CREATE: if we sent agent_input_schema but the server
		// response shows it empty, the server's CREATE handler dropped it
		// (older Studio versions before the create.go fix). Auto-retry via
		// UPDATE which is known to persist the field.
		if cfg.AgentInputSchema != "" && created.AgentInputSchema == "" {
			fmt.Printf("  WARN: CREATE didn't persist agent_input_schema (server version pre-fix?), applying via UPDATE\n")
			updatePayload, _ := json.Marshal(playbookUpdatePayload(cfg))
			rb, st, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d", pbID), updatePayload, orgID)
			if err != nil || st >= 300 {
				return provisionAPIErr(fmt.Sprintf("update playbook %q (verify-after-create retry)", cfg.Name), st, rb, err)
			}
		}
	}

	return reconcilePlaybookSteps(c, orgID, pbID, cfg.Steps)
}

// playbookCreatePayload maps the YAML config to the POST /api/playbooks body.
func playbookCreatePayload(cfg playbookConfig) map[string]interface{} {
	out := map[string]interface{}{
		"name":         cfg.Name,
		"description":  cfg.Description,
		"trigger_type": defaultStr(cfg.TriggerType, "manual"),
		"output_key":   defaultStr(cfg.OutputKey, "summary"),
	}
	if cfg.Schedule != nil {
		out["schedule"] = *cfg.Schedule
	}
	if cfg.AgentInputSchema != "" {
		out["agent_input_schema"] = cfg.AgentInputSchema
	}
	return out
}

// playbookUpdatePayload maps to PUT /api/playbooks/{id}.
func playbookUpdatePayload(cfg playbookConfig) map[string]interface{} {
	out := map[string]interface{}{
		"name":              cfg.Name,
		"description":       cfg.Description,
		"trigger_type":      defaultStr(cfg.TriggerType, "manual"),
		"output_key":        defaultStr(cfg.OutputKey, "summary"),
		"schedule_timezone": defaultStr(cfg.ScheduleTimezone, "UTC"),
	}
	if cfg.Schedule != nil {
		out["schedule"] = *cfg.Schedule
	}
	if cfg.Enabled != nil {
		out["enabled"] = *cfg.Enabled
	}
	if cfg.AgentTriggerable != nil {
		out["agent_triggerable"] = *cfg.AgentTriggerable
	}
	if cfg.AgentInputSchema != "" {
		out["agent_input_schema"] = cfg.AgentInputSchema
	}
	return out
}

// reconcilePlaybookSteps syncs the steps list to match cfg.Steps:
//   - existing step with same name → UPDATE in place
//   - new step in YAML → CREATE
//   - step on remote not in YAML → DELETE
func reconcilePlaybookSteps(c *provisionClient, orgID, pbID uint, want []playbookStep) error {
	body, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d/steps", pbID), orgID)
	if err != nil || status != 200 {
		return provisionAPIErr(fmt.Sprintf("list steps for playbook %d", pbID), status, body, err)
	}
	var existing []provisionPlaybookStepRemote
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return fmt.Errorf("parse steps for playbook %d: %w (body=%s)", pbID, err, provisionSummarize(body))
	}
	byName := make(map[string]provisionPlaybookStepRemote, len(existing))
	for _, s := range existing {
		byName[strings.ToLower(s.Name)] = s
	}

	var credByName map[string]uint
	wantNames := make(map[string]bool, len(want))

	for i, step := range want {
		key := strings.ToLower(step.Name)
		if wantNames[key] {
			return fmt.Errorf("playbook %d: duplicate step name %q in YAML — step names must be unique within a playbook", pbID, step.Name)
		}
		wantNames[key] = true
		// Position is auto-derived from array index; explicit position in YAML is ignored.
		step.Position = i

		var resolvedConfig any
		if c.dryRun {
			resolvedConfig = step.Config
		} else {
			resolvedConfig, err = resolveCredentialRefInConfig(step.Config, func() (map[string]uint, error) {
				if credByName != nil {
					return credByName, nil
				}
				m, err := listCredentialsByName(c, orgID)
				if err != nil {
					return nil, err
				}
				credByName = m
				return m, nil
			})
		}
		if err != nil {
			return fmt.Errorf("playbook %d step %q: %w", pbID, step.Name, err)
		}

		configJSON, err := stepConfigToJSON(resolvedConfig)
		if err != nil {
			return fmt.Errorf("playbook %d step %q: %w", pbID, step.Name, err)
		}
		enabled := true
		if step.Enabled != nil {
			enabled = *step.Enabled
		}
		payload := map[string]interface{}{
			"name":       step.Name,
			"step_type":  step.StepType,
			"position":   step.Position,
			"config":     configJSON,
			"output_key": defaultStr(step.OutputKey, "output"),
			"enabled":    enabled,
		}
		payloadBytes, _ := json.Marshal(payload)

		if cur, found := byName[key]; found {
			fmt.Printf("  UPDATE step %q id=%d position=%d\n", step.Name, cur.ID, step.Position)
			respBody, status, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d/steps/%d", pbID, cur.ID), payloadBytes, orgID)
			if err != nil || status >= 300 {
				return provisionAPIErr(fmt.Sprintf("update step %q on playbook %d", step.Name, pbID), status, respBody, err)
			}
		} else {
			fmt.Printf("  CREATE step %q position=%d\n", step.Name, step.Position)
			respBody, status, err := c.writeForOrg("POST", fmt.Sprintf("/playbooks/%d/steps", pbID), payloadBytes, orgID)
			if err != nil || status >= 300 {
				return provisionAPIErr(fmt.Sprintf("create step %q on playbook %d", step.Name, pbID), status, respBody, err)
			}
		}
	}

	// Reconcile deletions: step exists on remote but not in YAML.
	for _, cur := range existing {
		if !wantNames[strings.ToLower(cur.Name)] {
			fmt.Printf("  DELETE step %q id=%d (not in YAML)\n", cur.Name, cur.ID)
			respBody, status, err := c.writeForOrg("DELETE", fmt.Sprintf("/playbooks/%d/steps/%d", pbID, cur.ID), nil, orgID)
			if err != nil || (status >= 300 && status != 404) {
				return provisionAPIErr(fmt.Sprintf("delete step %q on playbook %d", cur.Name, pbID), status, respBody, err)
			}
		}
	}
	return nil
}

// listCredentialsByName fetches the org's credentials and returns a
// case-insensitive name → ID map. Cached at the call site.
func listCredentialsByName(c *provisionClient, orgID uint) (map[string]uint, error) {
	body, status, err := c.getForOrg("/credentials/", orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr("list credentials", status, body, err)
	}
	var creds []provisionCredentialListItem
	if err := unmarshalListEnvelope(body, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w (body=%s)", err, provisionSummarize(body))
	}
	firstName := make(map[string]string, len(creds))
	out := make(map[string]uint, len(creds))
	for _, cr := range creds {
		key := strings.ToLower(cr.Name)
		if existing, dup := firstName[key]; dup {
			return nil, fmt.Errorf("credential name collision (case-insensitive): %q (id=%d) and %q (id=%d) — rename one before applying", existing, out[key], cr.Name, cr.ID)
		}
		firstName[key] = cr.Name
		out[key] = cr.ID
	}
	return out, nil
}

// resolveCredentialRefInConfig substitutes `credential_ref: <name>` with
// `credential_id: <int>` by looking up the name in the org's credentials.
func resolveCredentialRefInConfig(cfg any, fetchCreds func() (map[string]uint, error)) (any, error) {
	if cfg == nil {
		return cfg, nil
	}
	normalised := convertYAMLValue(cfg)
	m, ok := normalised.(map[string]any)
	if !ok {
		return cfg, nil
	}
	refRaw, hasRef := m["credential_ref"]
	if !hasRef {
		return cfg, nil
	}
	if _, hasID := m["credential_id"]; hasID {
		return nil, fmt.Errorf("credential_ref and credential_id are both set — pick one")
	}
	ref, ok := refRaw.(string)
	if !ok || ref == "" {
		return nil, fmt.Errorf("credential_ref must be a non-empty string, got %T (%v)", refRaw, refRaw)
	}

	credByName, err := fetchCreds()
	if err != nil {
		return nil, fmt.Errorf("resolve credential_ref %q: %w", ref, err)
	}
	id, found := credByName[strings.ToLower(ref)]
	if !found {
		const maxListed = 5
		available := make([]string, 0, len(credByName))
		for name := range credByName {
			available = append(available, name)
		}
		sort.Strings(available)
		more := ""
		if len(available) > maxListed {
			available = available[:maxListed]
			more = fmt.Sprintf(" (+%d more)", len(credByName)-maxListed)
		}
		return nil, fmt.Errorf("credential_ref %q: not found in org. Apply credentials first, or check the name. Available: %v%s", ref, available, more)
	}

	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "credential_ref" {
			continue
		}
		out[k] = v
	}
	out["credential_id"] = id
	return out, nil
}

// stepConfigToJSON turns a YAML-decoded step config into the JSON string
// shape the API expects on the wire (see PlaybookStep.Config = "{...}").
func stepConfigToJSON(cfg interface{}) (string, error) {
	if cfg == nil {
		return "{}", nil
	}
	normalised := convertYAMLValue(cfg)
	b, err := json.Marshal(normalised)
	if err != nil {
		return "", fmt.Errorf("marshal step config: %w", err)
	}
	return string(b), nil
}

// convertYAMLValue normalises gopkg.in/yaml.v3-decoded values into JSON-safe types.
func convertYAMLValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, vv := range x {
			out[k] = convertYAMLValue(vv)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, vv := range x {
			out[fmt.Sprintf("%v", k)] = convertYAMLValue(vv)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, vv := range x {
			out[i] = convertYAMLValue(vv)
		}
		return out
	default:
		return v
	}
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

