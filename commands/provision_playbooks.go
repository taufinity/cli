// provision_playbooks.go — provision support for playbook resources.
//
// Mirrors the dashboards pattern: list, match by slug/name, diff against the
// live state, then upsert and reconcile child rows (steps). The diff is real
// (see provision_playbooks_diff.go): an unchanged playbook prints NOOP and
// issues no writes, and a diff that would silently revert live config (deleted
// steps, AI model swaps, schedule/enabled flips, error-policy changes) is
// classified HIGH drift and refused unless --allow-drift is passed.
//
// Versioning: every PUT/POST hits the standard /api/playbooks endpoints, which
// snapshot an entity version internally. The X-Change-Source: provision header
// is set automatically by the client (see provision_client.go), so version rows
// are attributable to provision rather than to a user.
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
// Schema example (studio/playbooks/<slug>.yaml):
//
//	name: Weekly digest
//	description: Summarises the week and posts it
//	trigger_type: schedule        # manual | event | schedule
//	schedule: "0 9 * * 1"
//	schedule_paused: false
//	output_key: digest
//	enabled: true
//	agent_triggerable: false
//	steps:
//	  - name: Collect
//	    step_type: llm_extract
//	    output_key: items
//	    enabled: true
//	    config:                   # arbitrary JSON, passed through to the step
//	      model: some-model-id
//	    error_policy:
//	      action: skip            # abort (default) | skip | use_default
//
// Notes:
//   - `config` is marshalled to a JSON string before sending — the step handler
//     stores a string-encoded JSON column.
//   - Steps are matched by name (nameless steps by position ordinal — see
//     stepKeyOf); new steps are POSTed, removed ones DELETEd. Array order in the
//     YAML is the source of truth for step ordering.
type playbookConfig struct {
	Name             string         `yaml:"name"`
	Slug             string         `yaml:"slug,omitempty"` // stable provision key; leading over name when set
	Description      string         `yaml:"description,omitempty"`
	TriggerType      string         `yaml:"trigger_type,omitempty"`
	Schedule         *string        `yaml:"schedule,omitempty"`
	ScheduleTimezone string         `yaml:"schedule_timezone,omitempty"`
	SchedulePaused   *bool          `yaml:"schedule_paused,omitempty"` // pause the cron without removing the schedule
	OutputKey        string         `yaml:"output_key,omitempty"`
	Enabled          *bool          `yaml:"enabled,omitempty"`
	AgentTriggerable *bool          `yaml:"agent_triggerable,omitempty"`
	AgentInputSchema string         `yaml:"agent_input_schema,omitempty"`
	Steps            []playbookStep `yaml:"steps,omitempty"`
	// Verification is documentation-only (a human handover checklist). Never sent
	// to the Studio API — declared here so the strict YAML parser doesn't reject it.
	Verification interface{} `yaml:"verification,omitempty"`
}

// playbookStep is the YAML form of a playbook step. `config` is arbitrary
// YAML/JSON (whatever the step type accepts) — provision marshals it to a JSON
// string before sending.
//
// KEEP THIS IN SYNC WITH THE SERVER MODEL. provision decodes YAML strictly and
// sends a *full snapshot* of each step, so a field the server supports and this
// struct lacks is never a harmless no-op: either the strict decode hard-fails
// and provision can't run at all, or — for a field the server has and we don't
// send — the apply silently strips it from live config. When the server's step
// model gains a field, add it here or state explicitly why not.
//
// Deliberately not modelled: id, playbook_id (server-assigned).
type playbookStep struct {
	Name      string      `yaml:"name"`
	StepType  string      `yaml:"step_type"`
	Position  int         `yaml:"position,omitempty"` // deprecated: ignored by provision; derived from array index
	Config    interface{} `yaml:"config,omitempty"`
	OutputKey string      `yaml:"output_key,omitempty"`
	Enabled   *bool       `yaml:"enabled,omitempty"`
	// ErrorPolicy controls what happens when the step fails: abort (default),
	// skip, or use_default (copy default_key's value into output_key and carry
	// on). Pointer so an absent policy is distinguishable from an explicit one.
	ErrorPolicy *provisionErrorPolicy `yaml:"error_policy,omitempty"`
}

// Error-policy actions, mirroring the server's step model.
const (
	provisionErrorPolicyAbort      = "abort"       // stop the run immediately (default)
	provisionErrorPolicySkip       = "skip"        // log and continue without writing output
	provisionErrorPolicyUseDefault = "use_default" // copy default_key's value to output_key, then continue
)

// provisionErrorPolicy is the per-step error-handling policy. The wire shape
// (json tags) must match the server's step create/update request exactly — it is
// sent verbatim in the step payload and read back from the step list.
type provisionErrorPolicy struct {
	Action     string `yaml:"action" json:"action"`
	DefaultKey string `yaml:"default_key,omitempty" json:"default_key,omitempty"`
}

// provisionPlaybookListItem is the minimal shape we need from GET /api/playbooks/.
type provisionPlaybookListItem struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// provisionPlaybookStepRemote is the API shape for an existing step.
type provisionPlaybookStepRemote struct {
	ID          uint                  `json:"id"`
	Name        string                `json:"name"`
	StepType    string                `json:"step_type"`
	Position    int                   `json:"position"`
	Config      string                `json:"config"`
	OutputKey   string                `json:"output_key"`
	Enabled     bool                  `json:"enabled"`
	ErrorPolicy *provisionErrorPolicy `json:"error_policy,omitempty"`
}

// applyPlaybooks applies all playbook YAML files from dir to the org.
// Supports a single playbook.yaml at the root and/or a playbooks/ subdirectory.
//
// allowDrift lets the operator opt in to a HIGH-drift apply (see updatePlaybook).
func applyPlaybooks(c *provisionClient, dir string, orgID uint, allowDrift bool) error {
	// single playbook.yaml at root
	if pf := filepath.Join(dir, "playbook.yaml"); fileExists(pf) {
		var cfg playbookConfig
		mustReadYAML(pf, &cfg)
		if err := upsertPlaybook(c, orgID, cfg, pf, allowDrift); err != nil {
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
			if err := upsertPlaybook(c, orgID, cfg, pf, allowDrift); err != nil {
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
func upsertPlaybook(c *provisionClient, orgID uint, cfg playbookConfig, yamlPath string, allowDrift bool) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("playbook: name is required")
	}
	noteUnnamedSteps(cfg)

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

	if len(matches) == 1 {
		return updatePlaybook(c, orgID, cfg, matches[0].ID, allowDrift)
	}
	return createPlaybook(c, orgID, cfg, yamlPath)
}

// createPlaybook is the apply path for a playbook that doesn't exist yet. No
// drift gate applies: there is no live config to overwrite.
func createPlaybook(c *provisionClient, orgID uint, cfg playbookConfig, yamlPath string) error {
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
	pbID := created.ID
	if created.Slug != "" {
		if err := pinSlug(yamlPath, cfg.Slug, created.Slug); err != nil {
			fmt.Printf("  WARN: could not pin slug for playbook %q: %v\n", cfg.Name, err)
		}
	}

	// Verify-after-CREATE: if we sent agent_input_schema but the server response
	// shows it empty, the server's CREATE handler dropped it (older Studio
	// versions). Auto-retry via UPDATE, which is known to persist the field.
	// This defends only agent_input_schema; don't read it as a general guarantee
	// that every field survives CREATE.
	if cfg.AgentInputSchema != "" && created.AgentInputSchema == "" {
		fmt.Printf("  WARN: CREATE didn't persist agent_input_schema (older server?), applying via UPDATE\n")
		updatePayload, _ := json.Marshal(playbookUpdatePayload(cfg))
		rb, st, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d", pbID), updatePayload, orgID)
		if err != nil || st >= 300 {
			return provisionAPIErr(fmt.Sprintf("update playbook %q (verify-after-create retry)", cfg.Name), st, rb, err)
		}
	}

	if err := reconcilePlaybookSteps(c, orgID, pbID, cfg.Steps); err != nil {
		return err
	}
	if cfg.SchedulePaused != nil && *cfg.SchedulePaused {
		return applySchedulePaused(c, orgID, pbID, true)
	}
	return nil
}

// updatePlaybook is the apply path for a playbook that already exists in Studio.
// Unlike an unconditional PUT it first diffs local against live:
//
//   - no difference          → NOOP, zero writes (no pointless version row)
//   - LOW drift              → UPDATE, writing only what actually changed
//   - HIGH drift             → loud warning + refuse, unless allowDrift
//   - HIGH drift + --dry-run → warning only, never refuses (a dry-run must
//     always be safe to run)
//
// If the remote state can't be read we degrade to the historical behaviour (blind
// PUT) with a warning, rather than blocking a deploy on a read failure.
func updatePlaybook(c *provisionClient, orgID uint, cfg playbookConfig, pbID uint, allowDrift bool) error {
	desired, err := buildDesiredSteps(c, orgID, pbID, cfg.Steps)
	if err != nil {
		return err
	}

	detail, remoteSteps, err := fetchPlaybookState(c, orgID, pbID)
	if err != nil {
		c.Warn("playbook %q id=%d: could not read live state for drift check (%v) — applying without a diff", cfg.Name, pbID, err)
		return applyPlaybookUpdate(c, orgID, cfg, pbID, desired, nil, nil)
	}

	diff := diffPlaybook(cfg, desired, *detail, remoteSteps)
	if diff.empty() {
		fmt.Printf("NOOP   playbook %q id=%d\n", cfg.Name, pbID)
		return nil
	}

	severity, reasons := diff.severity()
	if severity == driftHigh {
		printHighDriftWarning(c, orgID, cfg, pbID, diff, reasons)
		if !c.dryRun && !allowDrift {
			return fmt.Errorf("playbook %q id=%d: HIGH drift — refusing to apply. Pull first (taufinity provision pull playbooks) if your local file is stale, or re-run with --allow-drift to overwrite live config", cfg.Name, pbID)
		}
	}

	printPlaybookDiff(cfg, pbID, diff, severity)
	return applyPlaybookUpdate(c, orgID, cfg, pbID, desired, remoteSteps, &diff)
}

// applyPlaybookUpdate writes the diff. When diff is nil (remote state
// unreadable) it falls back to writing everything, which is what apply did
// before drift detection existed.
func applyPlaybookUpdate(c *provisionClient, orgID uint, cfg playbookConfig, pbID uint, desired []desiredStep, remoteSteps []provisionPlaybookStepRemote, diff *playbookDiff) error {
	if diff == nil || len(diff.Fields) > 0 {
		payload, _ := json.Marshal(playbookUpdatePayload(cfg))
		respBody, status, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d", pbID), payload, orgID)
		if err != nil || status >= 300 {
			return provisionAPIErr(fmt.Sprintf("update playbook %q", cfg.Name), status, respBody, err)
		}
	}
	if diff == nil {
		if cfg.SchedulePaused != nil {
			if err := applySchedulePaused(c, orgID, pbID, *cfg.SchedulePaused); err != nil {
				return err
			}
		}
		return reconcilePlaybookSteps(c, orgID, pbID, cfg.Steps)
	}
	for _, f := range diff.Fields {
		if f.Path == "schedule_paused" && cfg.SchedulePaused != nil {
			if err := applySchedulePaused(c, orgID, pbID, *cfg.SchedulePaused); err != nil {
				return err
			}
		}
	}
	return applyStepDiff(c, orgID, pbID, desired, diff.Steps)
}

// applySchedulePaused pauses or resumes a playbook's cron via
// POST /api/playbooks/{id}/schedule/pause. It is a separate endpoint rather than
// a PUT field because pausing cancels the pending scheduled task and resuming
// resets the consecutive-failure counter — the playbook update handler has no
// schedule_paused field at all, so a value sent there is silently dropped.
func applySchedulePaused(c *provisionClient, orgID, pbID uint, paused bool) error {
	payload, _ := json.Marshal(map[string]bool{"paused": paused})
	body, status, err := c.writeForOrg("POST", fmt.Sprintf("/playbooks/%d/schedule/pause", pbID), payload, orgID)
	if err != nil || status >= 300 {
		return provisionAPIErr(fmt.Sprintf("set schedule_paused=%v on playbook %d", paused, pbID), status, body, err)
	}
	return nil
}

// noteUnnamedSteps points out that a playbook has steps with no name. provision
// handles these (they match on position ordinal — see stepKeyOf), but the name is
// how a human identifies a step in the Studio UI and in drift output, so a
// nameless step degrades every message about it to "unnamed#2".
//
// Deliberately a plain advisory line and not c.Warn: it must not trip --strict
// and fail a CI apply over a pre-existing data-quality wart that provision now
// tolerates correctly. The durable fix is to require a step name server-side.
func noteUnnamedSteps(cfg playbookConfig) {
	n := 0
	for _, s := range cfg.Steps {
		if isUnnamedStep(s.Name) {
			n++
		}
	}
	if n > 0 {
		fmt.Printf("  NOTE: playbook %q has %d step(s) with no name — matched by position; name them to get readable diffs\n", cfg.Name, n)
	}
}

// printPlaybookDiff mirrors the dashboards output style: one UPDATE line with
// the changed playbook fields, then the per-step changes.
func printPlaybookDiff(cfg playbookConfig, pbID uint, diff playbookDiff, severity driftSeverity) {
	var names []string
	for _, f := range diff.Fields {
		names = append(names, f.Path)
	}
	if len(names) > 0 {
		fmt.Printf("UPDATE playbook %q id=%d [%s]\n", cfg.Name, pbID, strings.Join(names, ", "))
	} else {
		fmt.Printf("UPDATE playbook %q id=%d [steps only]\n", cfg.Name, pbID)
	}
	fmt.Printf("DRIFT  playbook=%q id=%d severity=%s\n", cfg.Name, pbID, severity)
	for _, f := range diff.Fields {
		fmt.Printf("  %s\n", f)
	}
	for _, s := range diff.Steps {
		switch s.Kind {
		case stepAdded:
			fmt.Printf("  ADD    step %q\n", s.label())
		case stepRemoved:
			fmt.Printf("  DELETE step %q id=%d (not in YAML)\n", s.label(), s.RemoteID)
		default:
			fmt.Printf("  MODIFY step %q id=%d\n", s.label(), s.RemoteID)
			for _, ch := range s.Changes {
				fmt.Printf("      %s\n", ch)
			}
		}
	}
}

// printHighDriftWarning explains exactly what would be overwritten, why it is
// flagged, and — because playbooks are entity-versioned — how to undo the apply
// if it turns out the local file was the stale one. A refusal the operator can
// act on beats a refusal they can only bypass.
func printHighDriftWarning(c *provisionClient, orgID uint, cfg playbookConfig, pbID uint, diff playbookDiff, reasons []string) {
	label := cfg.Slug
	if label == "" {
		label = cfg.Name
	}
	fmt.Printf("\nHIGH DRIFT: playbook %q id=%d\n", label, pbID)
	for _, f := range diff.Fields {
		fmt.Printf("    %s\n", f)
	}
	for _, s := range diff.Steps {
		switch s.Kind {
		case stepAdded:
			fmt.Printf("    step %q: would be ADDED\n", s.label())
		case stepRemoved:
			fmt.Printf("    step %q id=%d: would be DELETED (not present in local YAML)\n", s.label(), s.RemoteID)
		default:
			for _, ch := range s.Changes {
				fmt.Printf("    step %q: %s\n", s.label(), ch)
			}
		}
	}
	fmt.Printf("    Flagged because: %s\n", strings.Join(reasons, "; "))
	fmt.Printf("    This apply would OVERWRITE live config. If your local file is stale, pull first:\n")
	fmt.Printf("        taufinity provision pull playbooks --dir <dir> --org <slug>\n")
	if v := fetchPlaybookVersion(c, orgID, pbID); v > 0 {
		// Caveat, stated plainly because it is easy to over-read this hint: the
		// server's revert restores the playbook's own fields, not its step rows.
		// A DELETED step is NOT brought back by a revert — recover it from the
		// version history or re-add it by hand.
		fmt.Printf("    Current remote version: %d. To undo the playbook-level changes of an apply:\n", v)
		fmt.Printf("        POST /api/playbooks/%d/versions/%d/revert\n", pbID, v)
		fmt.Printf("    Note: a revert restores playbook fields only — it does NOT restore deleted steps.\n")
	} else {
		fmt.Printf("    Could not read the current version number — check the playbook's version history in Studio before applying.\n")
	}
	fmt.Printf("    To proceed anyway: --allow-drift\n\n")
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
	// schedule_paused is not a field on the create request either — it is applied
	// via the dedicated pause endpoint once the row exists (see createPlaybook).
	if cfg.AgentInputSchema != "" {
		out["agent_input_schema"] = cfg.AgentInputSchema
	}
	return out
}

// playbookUpdatePayload maps to PUT /api/playbooks/{id}. A full snapshot is sent
// so the payload is predictable and the diff can compare like with like.
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
	// schedule_paused is deliberately NOT in this payload: the playbook update
	// handler has no such field, so sending it here is silently dropped. Pausing
	// has scheduling side effects (the next scheduled task is cancelled, the
	// failure counter resets on resume), so it has its own endpoint — provision
	// applies it via applySchedulePaused.
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

// desiredStep is the fully-resolved step payload apply would send: credential
// refs already substituted, config already marshalled to the JSON string the API
// stores. Diffing against this (rather than against the raw YAML) means the diff
// compares like with like — what would go on the wire vs what is on the wire.
type desiredStep struct {
	Name        string
	StepType    string
	Position    int
	ConfigJSON  string
	OutputKey   string
	Enabled     bool
	ErrorPolicy *provisionErrorPolicy
}

func (s desiredStep) payload() []byte {
	// error_policy is always sent, including as an explicit null. provision
	// applies a full snapshot of each step, so removing the policy from the YAML
	// must remove it from the live step — omitting the key entirely would leave a
	// stale policy in place, and never sending the key at all (the previous
	// behaviour) meant a full-snapshot update wiped policies that were set
	// elsewhere.
	b, _ := json.Marshal(map[string]interface{}{
		"name":         s.Name,
		"step_type":    s.StepType,
		"position":     s.Position,
		"config":       s.ConfigJSON,
		"output_key":   s.OutputKey,
		"enabled":      s.Enabled,
		"error_policy": s.ErrorPolicy,
	})
	return b
}

// buildDesiredSteps turns the YAML steps into the payloads apply would send.
//
// Step configs may contain `credential_ref: "<name>"` instead of a numeric
// `credential_id`. The reference is resolved here against the org's credentials
// so the on-disk config never carries env-specific IDs and the same YAML applies
// to local and prod without manual edits.
//
// In dry-run, credential resolution is best-effort: a credential being created in
// the same run doesn't exist yet, so a failed lookup falls back to the raw config
// rather than aborting. Resolution is still attempted so a dry-run diff compares
// resolved against resolved and doesn't report a phantom
// credential_ref → credential_id change.
func buildDesiredSteps(c *provisionClient, orgID, pbID uint, want []playbookStep) ([]desiredStep, error) {
	// Lazy: only fetch the credential map if a step actually uses credential_ref.
	var credByName map[string]uint
	fetchCreds := func() (map[string]uint, error) {
		if credByName != nil {
			return credByName, nil
		}
		m, err := listCredentialsByName(c, orgID)
		if err != nil {
			return nil, err
		}
		credByName = m
		return m, nil
	}

	// Duplicate detection keys on the *name*, and only for steps that have one.
	// Nameless steps are matched by position ordinal (see stepKeyOf) and can
	// therefore never collide — previously they all hashed to "" and the second
	// one aborted the whole playbook, which made playbooks with unnamed steps
	// impossible to provision at all.
	seen := make(map[string]bool, len(want))
	out := make([]desiredStep, 0, len(want))
	for i, step := range want {
		// A nameless step is identified in messages by its index: %q of an empty
		// name tells the reader nothing.
		label := step.Name
		if isUnnamedStep(step.Name) {
			label = fmt.Sprintf("#%d (unnamed)", i)
		} else {
			key := strings.ToLower(strings.TrimSpace(step.Name))
			if seen[key] {
				return nil, fmt.Errorf("playbook %d: duplicate step name %q in YAML — step names must be unique within a playbook", pbID, step.Name)
			}
			seen[key] = true
		}

		resolvedConfig, err := resolveCredentialRefInConfig(step.Config, fetchCreds)
		if err != nil {
			if !c.dryRun {
				return nil, fmt.Errorf("playbook %d step %s: %w", pbID, label, err)
			}
			c.Warn("[dry-run] playbook %d step %s: credential_ref unresolved (%v) — diffing against the raw config", pbID, label, err)
			resolvedConfig = step.Config
		}
		configJSON, err := stepConfigToJSON(resolvedConfig)
		if err != nil {
			return nil, fmt.Errorf("playbook %d step %s: %w", pbID, label, err)
		}
		enabled := true
		if step.Enabled != nil {
			enabled = *step.Enabled
		}
		out = append(out, desiredStep{
			Name:     step.Name,
			StepType: step.StepType,
			// Position is auto-derived from array index; explicit position in YAML is ignored.
			Position:    i,
			ConfigJSON:  configJSON,
			OutputKey:   defaultStr(step.OutputKey, "output"),
			Enabled:     enabled,
			ErrorPolicy: step.ErrorPolicy,
		})
	}
	return out, nil
}

// fetchPlaybookSteps reads the live steps of a playbook.
func fetchPlaybookSteps(c *provisionClient, orgID, pbID uint) ([]provisionPlaybookStepRemote, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d/steps", pbID), orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr(fmt.Sprintf("list steps for playbook %d", pbID), status, body, err)
	}
	var existing []provisionPlaybookStepRemote
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return nil, fmt.Errorf("parse steps for playbook %d: %w (body=%s)", pbID, err, provisionSummarize(body))
	}
	return existing, nil
}

// fetchPlaybookState reads the live playbook + its steps — the "remote" side of
// the diff.
func fetchPlaybookState(c *provisionClient, orgID, pbID uint) (*playbookDetailRemote, []provisionPlaybookStepRemote, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d", pbID), orgID)
	if err != nil || status != 200 {
		return nil, nil, provisionAPIErr(fmt.Sprintf("get playbook %d", pbID), status, body, err)
	}
	var detail playbookDetailRemote
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, nil, fmt.Errorf("parse playbook %d: %w (body=%s)", pbID, err, provisionSummarize(body))
	}
	steps, err := fetchPlaybookSteps(c, orgID, pbID)
	if err != nil {
		return nil, nil, err
	}
	return &detail, steps, nil
}

// fetchPlaybookVersion returns the current (highest) entity-version number of a
// playbook, so a drift warning can print a concrete revert command. Returns 0
// when the version history can't be read — degrade gracefully; never abort an
// apply because an advisory lookup failed.
//
// The versions endpoint returns the full history; we take the max rather than
// trusting the ordering.
func fetchPlaybookVersion(c *provisionClient, orgID, pbID uint) int {
	body, status, err := c.getForOrg(fmt.Sprintf("/playbooks/%d/versions", pbID), orgID)
	if err != nil || status != 200 {
		return 0
	}
	var versions []struct {
		Version int `json:"version"`
	}
	if err := unmarshalListEnvelope(body, &versions); err != nil {
		return 0
	}
	max := 0
	for _, v := range versions {
		if v.Version > max {
			max = v.Version
		}
	}
	return max
}

// applyStepDiff writes only the steps that actually changed:
//   - step in YAML, not on remote → CREATE
//   - step differs                → UPDATE
//   - step on remote, not in YAML → DELETE
//
// Unchanged steps are not written at all: no wasted PUT, no version row for a
// no-op edit.
func applyStepDiff(c *provisionClient, orgID, pbID uint, desired []desiredStep, changes []stepDiff) error {
	// Key on stepDiff.Key, not on the name: nameless steps all share the name ""
	// and would otherwise resolve to the same (arbitrary) desiredStep, writing the
	// wrong config to the wrong step.
	keys := desiredStepKeys(desired)
	byKey := make(map[string]desiredStep, len(desired))
	for i, s := range desired {
		byKey[keys[i]] = s
	}
	for _, ch := range changes {
		switch ch.Kind {
		case stepRemoved:
			respBody, status, err := c.writeForOrg("DELETE", fmt.Sprintf("/playbooks/%d/steps/%d", pbID, ch.RemoteID), nil, orgID)
			if err != nil || (status >= 300 && status != 404) {
				return provisionAPIErr(fmt.Sprintf("delete step %q on playbook %d", ch.label(), pbID), status, respBody, err)
			}
		case stepAdded:
			step := byKey[ch.Key]
			respBody, status, err := c.writeForOrg("POST", fmt.Sprintf("/playbooks/%d/steps", pbID), step.payload(), orgID)
			if err != nil || status >= 300 {
				return provisionAPIErr(fmt.Sprintf("create step %q on playbook %d", ch.label(), pbID), status, respBody, err)
			}
		default:
			step := byKey[ch.Key]
			respBody, status, err := c.writeForOrg("PUT", fmt.Sprintf("/playbooks/%d/steps/%d", pbID, ch.RemoteID), step.payload(), orgID)
			if err != nil || status >= 300 {
				return provisionAPIErr(fmt.Sprintf("update step %q on playbook %d", ch.label(), pbID), status, respBody, err)
			}
		}
	}
	return nil
}

// reconcilePlaybookSteps syncs the steps of a freshly-CREATEd playbook (and is
// the fallback when the live state couldn't be read for a diff). It diffs against
// whatever is on the remote and writes only the delta.
func reconcilePlaybookSteps(c *provisionClient, orgID, pbID uint, want []playbookStep) error {
	desired, err := buildDesiredSteps(c, orgID, pbID, want)
	if err != nil {
		return err
	}
	remote, err := fetchPlaybookSteps(c, orgID, pbID)
	if err != nil {
		return err
	}
	changes := diffPlaybookSteps(desired, remote)
	for _, ch := range changes {
		switch ch.Kind {
		case stepAdded:
			fmt.Printf("  CREATE step %q\n", ch.label())
		case stepRemoved:
			fmt.Printf("  DELETE step %q id=%d (not in YAML)\n", ch.label(), ch.RemoteID)
		default:
			fmt.Printf("  UPDATE step %q id=%d\n", ch.label(), ch.RemoteID)
		}
	}
	return applyStepDiff(c, orgID, pbID, desired, changes)
}

// listCredentialsByName fetches the org's credentials and returns a
// case-insensitive name → ID map. Cached at the call site.
//
// Two credentials in the same org whose names differ only by case would silently
// collide in the lower-cased map and route credential_ref lookups to whichever
// the API returned last. Detect that and refuse to apply — the operator must
// rename one. The credentials API doesn't enforce case-insensitive uniqueness, so
// this is a real possibility for orgs that grew their credentials via the UI.
func listCredentialsByName(c *provisionClient, orgID uint) (map[string]uint, error) {
	body, status, err := c.getForOrg("/credentials/", orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr("list credentials", status, body, err)
	}
	var creds []provisionCredentialListItem
	if err := unmarshalListEnvelope(body, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w (body=%s)", err, provisionSummarize(body))
	}
	// Track the first (case-sensitive) name seen under each lower-cased key so the
	// collision message can name both originals.
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
//
// The returned config is a copy — the caller's input is never mutated.
//
// A step config may legitimately have neither field (a step that doesn't use
// credentials), or carry `credential_id` directly (env-specific override). Only
// `credential_ref` triggers a lookup, and the lookup is lazy via the fetchCreds
// closure so playbooks that never use refs pay no API cost.
func resolveCredentialRefInConfig(cfg any, fetchCreds func() (map[string]uint, error)) (any, error) {
	if cfg == nil {
		return cfg, nil
	}
	normalised := convertYAMLValue(cfg)
	m, ok := normalised.(map[string]any)
	if !ok {
		return cfg, nil // step config isn't an object — nothing to resolve
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
		// Cap and sort the listed names so the error is reproducible regardless of
		// map iteration order, and stays bounded — credential names are
		// operator-chosen and listing all of them in CI logs is needless exposure.
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

// stepConfigToJSON turns a YAML-decoded step config into the JSON string shape
// the API expects on the wire.
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

// convertYAMLValue normalises YAML-decoded values into JSON-safe types. yaml.v3
// already decodes map keys as strings (unlike v2's interface{}), but we convert
// defensively so both decoder versions work.
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
