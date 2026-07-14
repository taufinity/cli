// provision_playbooks_diff.go — diff, NOOP detection and drift classification
// for playbook apply.
//
// Why this exists: apply used to PUT unconditionally. A dry-run therefore
// printed UPDATE for every playbook, even one exported byte-for-byte from the
// same Studio seconds earlier — so it could never tell you whether your local
// YAML was stale. And a stale file silently reverted live config on apply: the
// full-snapshot payload rewrites every field, so anything edited in the Studio
// UI since the last pull is overwritten without a word.
//
// Two jobs here:
//  1. A real diff, so an unchanged playbook is a NOOP and issues no writes at
//     all (no pointless version row, no risk of a rewrite).
//  2. Classify the diff. Changes that silently remove or re-point live
//     behaviour (deleted steps, AI model/provider swaps, schedule/enabled
//     flips, error-policy changes, wholesale step rewrites) are HIGH drift and
//     refuse to apply without --allow-drift.
package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxModifiedStepsBeforeHighDrift is the number of modified steps above which
// the diff is treated as a wholesale rewrite rather than a targeted edit. A
// local file that differs from live in many steps at once is far more likely to
// be stale than deliberate.
const maxModifiedStepsBeforeHighDrift = 3

// changedStepFractionHighDrift: if more than this fraction of the remote steps
// are touched (added, removed or modified), treat as HIGH regardless of count.
const changedStepFractionHighDrift = 0.5

// aiModelConfigKeys are step-config keys that re-point which model or provider
// a step runs against. Overwriting these from a stale file is one of the
// nastiest failure modes: the playbook keeps running, but against the wrong (or
// a retired) model, and nothing about the apply looks unusual.
var aiModelConfigKeys = map[string]bool{
	"model":    true,
	"provider": true,
}

// highRiskPlaybookFields are playbook-level fields whose change alters when, or
// whether, the playbook runs at all.
var highRiskPlaybookFields = map[string]bool{
	"schedule":        true,
	"schedule_paused": true,
	"enabled":         true,
}

type driftSeverity int

const (
	driftNone driftSeverity = iota
	driftLow
	driftHigh
)

func (s driftSeverity) String() string {
	switch s {
	case driftHigh:
		return "HIGH"
	case driftLow:
		return "LOW"
	default:
		return "NONE"
	}
}

// fieldChange is one remote → local difference. Old is what Studio has now, New
// is what the apply would write.
type fieldChange struct {
	Path   string // "description", "config.model", "enabled"
	Old    string // current remote value
	New    string // value this apply would write
	Reason string // set when the change is flagged high-risk; empty otherwise
}

func (fc fieldChange) String() string {
	s := fmt.Sprintf("%s  %s -> %s", fc.Path, fc.Old, fc.New)
	if fc.Reason != "" {
		s += fmt.Sprintf("   (%s)", fc.Reason)
	}
	return s
}

// stepKind classifies a step-level change.
type stepKind string

const (
	stepAdded    stepKind = "ADD"
	stepRemoved  stepKind = "DELETE"
	stepModified stepKind = "MODIFY"
)

type stepDiff struct {
	Name     string
	Key      string // match key (see stepKeyOf) — identity used to pair local ↔ remote
	Kind     stepKind
	RemoteID uint // 0 for adds
	Changes  []fieldChange
}

// label is how a step is named in human-facing output. A nameless step has no
// name to print (`step ""` is useless in a drift message), so it falls back to
// the synthetic key we actually matched on — `unnamed#2` — which points the
// reader at the right entry in the UI's step list.
func (s stepDiff) label() string {
	if isUnnamedStep(s.Name) {
		return s.Key
	}
	return s.Name
}

func isUnnamedStep(name string) bool { return strings.TrimSpace(name) == "" }

// stepKeyOf computes the identity used to pair a local YAML step with a live
// remote step.
//
// Named steps key on their case-insensitive name — the name is how a human
// identifies a step, and it survives reordering.
//
// Nameless steps are the problem this exists to solve. The Studio UI never
// required a step name, so playbooks exist with empty-named steps. Keying those
// on the name collapses them all onto "", which provision rejected outright
// ("duplicate step name") — meaning those playbooks could not be provisioned at
// all.
//
// A nameless step has no identity except its place in the sequence, so that is
// what we key on: its ordinal among the *other nameless steps*, in position
// order. Two properties matter and both are deliberate:
//
//   - Ordinal, not raw position. Remote positions are frequently sparse (a step
//     was deleted and the rest were never renumbered), while local positions are
//     the contiguous array index. Keying on the raw value would mismatch every
//     step after a gap. The rank in the position-sorted list is stable across
//     that.
//   - Ordinal among nameless steps only, not among all steps. Inserting or
//     renaming a *named* step must not renumber the nameless ones out from under
//     themselves and turn a NOOP into a delete-and-recreate.
//
// Rejected alternative: round-tripping the server's step ID through the YAML
// (`step_id:`). It is the most precise key, but it bakes environment-specific
// IDs into files that are meant to be a portable source of truth. It would work
// for pull-then-apply-to-the-same-env and be actively wrong for cross-env
// provisioning (a staging step ID means nothing in production, and would either
// 404 or collide with an unrelated step). Position-ordinal keying carries no
// server state and applies identically to every environment.
//
// The honest limitation: reordering nameless steps relative to each other is
// indistinguishable from editing them in place. That is inherent — a step with
// no name has nothing else to be recognised by. The real fix is to require a
// name server-side; until then, noteUnnamedSteps nudges the operator to add one.
func stepKeyOf(name string, unnamedOrdinal int) string {
	if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
		return "name:" + n
	}
	return fmt.Sprintf("unnamed#%d", unnamedOrdinal)
}

// desiredStepKeys returns the match key for each desired step, index-aligned
// with desired. Desired steps are already in canonical order (position == index).
func desiredStepKeys(desired []desiredStep) []string {
	out := make([]string, len(desired))
	unnamed := 0
	for i, s := range desired {
		out[i] = stepKeyOf(s.Name, unnamed)
		if isUnnamedStep(s.Name) {
			unnamed++
		}
	}
	return out
}

// remoteStepKeys sorts the remote steps into canonical (position) order and
// returns them alongside their index-aligned match keys. Sorting is what makes
// the nameless ordinal meaningful: the API returns steps in no guaranteed order,
// and their positions may be sparse.
func remoteStepKeys(remote []provisionPlaybookStepRemote) ([]provisionPlaybookStepRemote, []string) {
	sorted := make([]provisionPlaybookStepRemote, len(remote))
	copy(sorted, remote)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Position < sorted[j].Position })

	keys := make([]string, len(sorted))
	unnamed := 0
	for i, s := range sorted {
		keys[i] = stepKeyOf(s.Name, unnamed)
		if isUnnamedStep(s.Name) {
			unnamed++
		}
	}
	return sorted, keys
}

// playbookDiff is the full remote → local delta for one playbook.
type playbookDiff struct {
	Fields          []fieldChange // playbook-level
	Steps           []stepDiff
	RemoteStepCount int
}

func (d playbookDiff) empty() bool { return len(d.Fields) == 0 && len(d.Steps) == 0 }

func (d playbookDiff) countKind(k stepKind) int {
	n := 0
	for _, s := range d.Steps {
		if s.Kind == k {
			n++
		}
	}
	return n
}

// significant reports whether a step change is more than cosmetic renumbering.
//
// Remote positions are frequently sparse (a step was deleted, or the YAML
// carries hand-written `position:` values with gaps), while provision derives
// contiguous positions from the array index. Rewriting 7,8,15,20 to 0,1,2,3
// keeps the exact same order — it is a numbering fix, not a behaviour change.
// Letting it count towards the drift thresholds would flag a large share of
// playbooks as HIGH on their first apply and train everyone to reach for
// --allow-drift reflexively, which defeats the gate. The write still happens;
// only the severity ignores it.
func (s stepDiff) significant() bool {
	if s.Kind != stepModified {
		return true
	}
	for _, c := range s.Changes {
		if c.Path != "position" {
			return true
		}
	}
	return false
}

func (d playbookDiff) significantSteps() []stepDiff {
	var out []stepDiff
	for _, s := range d.Steps {
		if s.significant() {
			out = append(out, s)
		}
	}
	return out
}

// severity classifies the diff and returns the human-readable reasons a HIGH
// classification was reached (empty for LOW/NONE).
func (d playbookDiff) severity() (driftSeverity, []string) {
	if d.empty() {
		return driftNone, nil
	}
	var reasons []string

	if n := d.countKind(stepRemoved); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d step(s) would be DELETED (removes live functionality)", n))
	}
	// Field-level risks (AI model/provider re-point, error-policy change) carry
	// their own Reason. Group them so the warning names each risk once with a count.
	riskCounts := map[string]int{}
	for _, s := range d.Steps {
		for _, c := range s.Changes {
			if c.Reason != "" {
				riskCounts[c.Reason]++
			}
		}
	}
	riskNames := make([]string, 0, len(riskCounts))
	for name := range riskCounts {
		riskNames = append(riskNames, name)
	}
	sort.Strings(riskNames)
	for _, name := range riskNames {
		reasons = append(reasons, fmt.Sprintf("%d field(s) would change: %s", riskCounts[name], name))
	}
	// Playbook-level fields that change when, or whether, it runs.
	var risky []string
	for _, f := range d.Fields {
		if highRiskPlaybookFields[f.Path] {
			risky = append(risky, f.Path)
		}
	}
	if len(risky) > 0 {
		reasons = append(reasons, fmt.Sprintf("run-control field(s) would change: %s", strings.Join(risky, ", ")))
	}

	significant := d.significantSteps()
	modified := 0
	for _, s := range significant {
		if s.Kind == stepModified {
			modified++
		}
	}
	if modified > maxModifiedStepsBeforeHighDrift {
		reasons = append(reasons, fmt.Sprintf("%d steps modified (more than %d — looks like a wholesale rewrite, not a targeted edit)",
			modified, maxModifiedStepsBeforeHighDrift))
	}
	// The fraction rule needs at least two touched steps: on a one- or two-step
	// playbook "more than half" is reached by any single edit, which is a
	// targeted change, not a rewrite.
	touched := len(significant)
	if touched > 1 && d.RemoteStepCount > 0 && float64(touched) > changedStepFractionHighDrift*float64(d.RemoteStepCount) {
		reasons = append(reasons, fmt.Sprintf("%d of %d remote steps touched (>%.0f%%)",
			touched, d.RemoteStepCount, changedStepFractionHighDrift*100))
	}

	if len(reasons) > 0 {
		return driftHigh, reasons
	}
	return driftLow, nil
}

// diffPlaybook compares the payloads this apply would send against the current
// remote state. `desired` is the fully-resolved step payload set (credential
// refs already substituted) so the comparison is against what actually goes on
// the wire, not against the raw YAML.
func diffPlaybook(cfg playbookConfig, desired []desiredStep, remote playbookDetailRemote, remoteSteps []provisionPlaybookStepRemote) playbookDiff {
	d := playbookDiff{RemoteStepCount: len(remoteSteps)}
	local := playbookUpdatePayload(cfg)
	// schedule_paused isn't part of the PUT body (the update handler has no such
	// field — see playbookUpdatePayload); it is applied through the dedicated
	// pause endpoint. It still belongs in the diff: flipping a schedule on or off
	// is exactly the kind of invisible run-behaviour change this gate exists for.
	if cfg.SchedulePaused != nil {
		local["schedule_paused"] = *cfg.SchedulePaused
	}
	d.Fields = diffPlaybookFields(local, remote)
	d.Steps = diffPlaybookSteps(desired, remoteSteps)
	return d
}

// diffPlaybookFields compares the PUT body against the remote detail. Only keys
// the payload actually sends are compared — a field omitted from the YAML is
// never sent, so it cannot drift.
func diffPlaybookFields(payload map[string]interface{}, remote playbookDetailRemote) []fieldChange {
	remoteSchedule := ""
	if remote.Schedule != nil {
		remoteSchedule = *remote.Schedule
	}
	// Remote values, with the same defaults playbookUpdatePayload applies to the
	// local side, so an unset remote column doesn't read as drift.
	remoteVals := map[string]interface{}{
		"name":               remote.Name,
		"description":        remote.Description,
		"trigger_type":       defaultStr(remote.TriggerType, "manual"),
		"output_key":         defaultStr(remote.OutputKey, "summary"),
		"schedule_timezone":  defaultStr(remote.ScheduleTimezone, "UTC"),
		"schedule":           remoteSchedule,
		"schedule_paused":    remote.SchedulePaused,
		"enabled":            remote.Enabled,
		"agent_triggerable":  remote.AgentTriggerable,
		"agent_input_schema": remote.AgentInputSchema,
	}

	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []fieldChange
	for _, k := range keys {
		rv, known := remoteVals[k]
		if !known {
			continue // field we send but can't read back — can't diff it
		}
		lv := payload[k]
		if k == "agent_input_schema" {
			// Semantically compare: whitespace and key order in a JSON schema
			// blob are not drift.
			if jsonStringsEqual(fmt.Sprintf("%v", rv), fmt.Sprintf("%v", lv)) {
				continue
			}
		} else if fmt.Sprintf("%v", rv) == fmt.Sprintf("%v", lv) {
			continue
		}
		out = append(out, fieldChange{Path: k, Old: fmtScalar(rv), New: fmtScalar(lv)})
	}
	return out
}

// diffPlaybookSteps matches steps by stepKeyOf — name for named steps, position
// ordinal for nameless ones — which is the same key apply uses to decide UPDATE
// vs CREATE, and reports adds, removes and per-field modifications.
func diffPlaybookSteps(desired []desiredStep, remote []provisionPlaybookStepRemote) []stepDiff {
	desiredKeys := desiredStepKeys(desired)
	remoteSorted, remoteKeys := remoteStepKeys(remote)

	byKey := make(map[string]provisionPlaybookStepRemote, len(remoteSorted))
	for i, s := range remoteSorted {
		byKey[remoteKeys[i]] = s
	}
	wanted := make(map[string]bool, len(desired))

	var out []stepDiff
	for i, want := range desired {
		key := desiredKeys[i]
		wanted[key] = true
		cur, found := byKey[key]
		if !found {
			out = append(out, stepDiff{Name: want.Name, Key: key, Kind: stepAdded})
			continue
		}
		changes := diffStepFields(cur, want)
		if len(changes) > 0 {
			out = append(out, stepDiff{Name: want.Name, Key: key, Kind: stepModified, RemoteID: cur.ID, Changes: changes})
		}
	}
	// Iterate the position-sorted remote so deletions are reported in playbook
	// order rather than whatever order the API happened to return.
	for i, cur := range remoteSorted {
		if !wanted[remoteKeys[i]] {
			out = append(out, stepDiff{Name: cur.Name, Key: remoteKeys[i], Kind: stepRemoved, RemoteID: cur.ID})
		}
	}
	return out
}

func diffStepFields(remote provisionPlaybookStepRemote, local desiredStep) []fieldChange {
	var out []fieldChange
	if remote.StepType != local.StepType {
		out = append(out, fieldChange{Path: "step_type", Old: remote.StepType, New: local.StepType})
	}
	if remote.Position != local.Position {
		out = append(out, fieldChange{Path: "position", Old: fmt.Sprint(remote.Position), New: fmt.Sprint(local.Position)})
	}
	if remote.OutputKey != local.OutputKey {
		out = append(out, fieldChange{Path: "output_key", Old: remote.OutputKey, New: local.OutputKey})
	}
	if remote.Enabled != local.Enabled {
		out = append(out, fieldChange{Path: "enabled", Old: fmt.Sprint(remote.Enabled), New: fmt.Sprint(local.Enabled)})
	}
	out = append(out, diffErrorPolicy(remote.ErrorPolicy, local.ErrorPolicy)...)
	out = append(out, diffStepConfig(remote.Config, local.ConfigJSON)...)
	return out
}

// diffErrorPolicy compares a step's error policy. The policy decides whether a
// step failure aborts the whole run, is skipped, or falls back to a default
// value — so changing or dropping it silently rewrites production failure
// behaviour. Any change to the action (including adding or removing the policy
// altogether) is flagged high-risk; a default_key tweak on an unchanged action
// is a normal edit.
func diffErrorPolicy(remote, local *provisionErrorPolicy) []fieldChange {
	switch {
	case remote == nil && local == nil:
		return nil
	case remote == nil:
		return []fieldChange{{
			Path: "error_policy", Old: "(absent)", New: fmtJSON(local),
			Reason: "error policy added (changes failure behaviour)",
		}}
	case local == nil:
		return []fieldChange{{
			Path: "error_policy", Old: fmtJSON(remote), New: "(absent)",
			Reason: "error policy removed (step would abort the run on failure)",
		}}
	}
	var out []fieldChange
	// An unset action means abort on both sides.
	ra := defaultStr(remote.Action, provisionErrorPolicyAbort)
	la := defaultStr(local.Action, provisionErrorPolicyAbort)
	if ra != la {
		out = append(out, fieldChange{
			Path: "error_policy.action", Old: ra, New: la,
			Reason: "error policy change (changes failure behaviour)",
		})
	}
	if remote.DefaultKey != local.DefaultKey {
		out = append(out, fieldChange{Path: "error_policy.default_key", Old: fmtScalar(remote.DefaultKey), New: fmtScalar(local.DefaultKey)})
	}
	return out
}

// diffStepConfig compares the two JSON-string step configs and returns one
// fieldChange per changed leaf, with a dotted path (config.model,
// config.retry.attempts) — so a model swap reports as one line rather than
// "config changed". Falls back to a whole-blob compare if either side isn't
// valid JSON.
func diffStepConfig(remoteJSON, localJSON string) []fieldChange {
	rv, rok := decodeStepConfigJSON(remoteJSON)
	lv, lok := decodeStepConfigJSON(localJSON)
	if !rok || !lok {
		if strings.TrimSpace(remoteJSON) == strings.TrimSpace(localJSON) {
			return nil
		}
		return []fieldChange{{Path: "config", Old: truncateValue(remoteJSON), New: truncateValue(localJSON)}}
	}
	changes := diffJSONValues("config", rv, lv)
	for i := range changes {
		if isAIModelPath(changes[i].Path) {
			changes[i].Reason = "AI model change"
		}
	}
	return changes
}

func decodeStepConfigJSON(s string) (any, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return nil, false
	}
	if v == nil {
		return map[string]any{}, true
	}
	return v, true
}

// diffJSONValues walks two decoded JSON values in parallel. Objects recurse
// key-by-key (so a one-key change reports as config.model, not "config
// changed"); everything else compares as a whole.
func diffJSONValues(path string, remote, local any) []fieldChange {
	rm, rok := remote.(map[string]any)
	lm, lok := local.(map[string]any)
	if rok && lok {
		keys := map[string]bool{}
		for k := range rm {
			keys[k] = true
		}
		for k := range lm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)

		var out []fieldChange
		for _, k := range sorted {
			rv, rhas := rm[k]
			lv, lhas := lm[k]
			sub := path + "." + k
			switch {
			case !rhas:
				out = append(out, fieldChange{Path: sub, Old: "(absent)", New: fmtJSON(lv)})
			case !lhas:
				out = append(out, fieldChange{Path: sub, Old: fmtJSON(rv), New: "(absent)"})
			default:
				out = append(out, diffJSONValues(sub, rv, lv)...)
			}
		}
		return out
	}
	if jsonDeepEqual(remote, local) {
		return nil
	}
	return []fieldChange{{Path: path, Old: fmtJSON(remote), New: fmtJSON(local)}}
}

// isAIModelPath reports whether the final segment of a config path names a
// model/provider field, at any nesting depth (config.model, config.llm.model...).
func isAIModelPath(path string) bool {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return false
	}
	return aiModelConfigKeys[path[idx+1:]]
}

func jsonDeepEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// jsonStringsEqual compares two JSON-bearing strings semantically, falling back
// to a literal compare when either side isn't valid JSON (e.g. empty).
func jsonStringsEqual(a, b string) bool {
	av, aok := decodeStepConfigJSON(a)
	bv, bok := decodeStepConfigJSON(b)
	if !aok || !bok {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return jsonDeepEqual(av, bv)
}

func fmtScalar(v any) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	if s == "" {
		return `""`
	}
	return truncateValue(s)
}

func fmtJSON(v any) string {
	if s, ok := v.(string); ok {
		return truncateValue(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncateValue(string(b))
}

// truncateValue keeps warning lines readable: prompt templates and schemas run
// to thousands of characters.
func truncateValue(s string) string {
	const max = 70
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", "\\n")
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return `""`
	}
	return s
}
