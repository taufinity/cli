// provision_sites_pipeline_diff.go — diff, NOOP detection and drift
// classification for a site's content pipeline.
//
// Why this exists: apply used to PUT the pipeline unconditionally, with
// mode=replace, discarding the GET it already made. Two consequences, both bad:
//
//  1. Every apply rewrote the whole pipeline even when nothing had changed, so a
//     dry-run could never tell you whether your local file was in sync.
//  2. mode=replace makes every step that is absent from the local file a
//     deletion. A stale or partial local pipeline.yaml therefore silently
//     replaced the live pipeline with the stale version — the whole content
//     pipeline of a site, not one step of it.
//
// Same treatment as the playbook path (provision_playbooks_diff.go), and it
// deliberately reuses that file's machinery — fieldChange, stepKind,
// driftSeverity, diffJSONValues, the modified-step threshold — rather than
// growing a second, divergent notion of what drift is. The shapes differ enough
// (pipeline steps have no server IDs, no error policy, and their identity is the
// step kind rather than a free-text name) that the diff itself is separate.
//
// What the remote side can and cannot tell us — this shapes the whole diff:
//
//   - GET returns a step's kind in the `type` field; the API stores it as `name`.
//     That is the step's identity here. Because a pipeline may run the same kind
//     twice (two quality_validation instances, say), the match key is the kind
//     plus its occurrence ordinal.
//   - GET *merges* the site-level AI defaults into every step's settings, and
//     *infers* provider/model from them when the step sets none. So a field the
//     local file does not declare will still come back populated. Comparing those
//     would report drift on every single apply, so provider, model and the
//     inherited settings keys are only compared when the local file declares
//     them.
//   - GET redacts secret-looking settings keys, so a locally-declared secret can
//     never round-trip. Those keys are skipped rather than reported as an
//     eternal add.
//   - GET does not return output_key at all, so a change to it is invisible to
//     this diff. Stated here so the gap is known rather than discovered.
package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// pipelineStepRemote is one live step as GET /api/sites/{id}/pipeline returns it.
// Type carries the step kind (the API stores it under `name`).
type pipelineStepRemote struct {
	Type     string                 `json:"type"`
	Enabled  bool                   `json:"enabled"`
	Provider string                 `json:"provider,omitempty"`
	Model    string                 `json:"model,omitempty"`
	Settings map[string]interface{} `json:"settings,omitempty"`
}

// pipelineRemote is the live pipeline. topic_discovery_steps is also returned by
// the API but provision never sends it, so it is not modelled: a field we do not
// write cannot drift.
type pipelineRemote struct {
	Steps []pipelineStepRemote `json:"steps"`
}

// pipelineInheritedSettingKeys are settings the API copies down from the site's
// AI config into every step before returning it. They appear on the remote side
// whether or not the step sets them, so a remote-only occurrence of one of these
// is not evidence that the local file dropped it.
var pipelineInheritedSettingKeys = map[string]bool{
	"temperature":     true,
	"max_tokens":      true,
	"min_human_score": true,
}

// pipelineRedactedSettingKeys mirrors the secret-looking keys the API strips from
// the settings it returns. A local step that declares one can never see it come
// back, so comparing it would report the same phantom change on every apply.
var pipelineRedactedSettingKeys = map[string]bool{
	"api_key": true, "apiKey": true,
	"api_secret": true, "apiSecret": true,
	"secret": true, "password": true, "token": true,
	"access_token": true, "refresh_token": true,
	"private_key": true, "encryption_key": true,
	"credentials": true, "auth": true, "authorization": true, "bearer": true,
}

// Reasons attached to a fieldChange that on its own makes the diff HIGH.
const (
	reasonPipelineModelChange = "AI model/provider change"
	reasonPipelineStepDisable = "step would be DISABLED (removes live behaviour)"
)

// pipelineStepDiff is one step-level change. Unlike playbook steps, pipeline
// steps have no server-side ID; a step is identified by its kind and, when the
// kind repeats, its occurrence.
type pipelineStepDiff struct {
	Kind    stepKind
	Key     string // match key: "<step kind>#<occurrence>"
	Label   string // step kind, with the occurrence appended when it repeats
	Changes []fieldChange
}

type pipelineDiff struct {
	Steps           []pipelineStepDiff
	RemoteStepCount int
	LocalStepCount  int
}

func (d pipelineDiff) empty() bool { return len(d.Steps) == 0 }

func (d pipelineDiff) countKind(k stepKind) int {
	n := 0
	for _, s := range d.Steps {
		if s.Kind == k {
			n++
		}
	}
	return n
}

// severity classifies the diff, returning the reasons a HIGH classification was
// reached (empty for LOW/NONE).
//
// HIGH is reserved for a diff that would destroy or re-point live behaviour:
// mode=replace turns any absent step into a deletion, a disabled step stops
// running, and a model/provider swap keeps the pipeline running against
// something else entirely. A prompt or threshold edit is a normal targeted change
// and stays LOW.
func (d pipelineDiff) severity() (driftSeverity, []string) {
	if d.empty() {
		return driftNone, nil
	}
	var reasons []string

	if n := d.countKind(stepRemoved); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d step(s) would be REMOVED — mode=replace deletes every step absent from the local file", n))
	}
	if d.LocalStepCount < d.RemoteStepCount {
		reasons = append(reasons, fmt.Sprintf("step count drops from %d to %d", d.RemoteStepCount, d.LocalStepCount))
	}

	// Field-level risks carry their own Reason. Group them so each risk is named
	// once, with a count.
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

	if n := d.countKind(stepModified); n > maxModifiedStepsBeforeHighDrift {
		reasons = append(reasons, fmt.Sprintf("%d steps modified (more than %d — looks like a wholesale rewrite, not a targeted edit)",
			n, maxModifiedStepsBeforeHighDrift))
	}

	if len(reasons) > 0 {
		return driftHigh, reasons
	}
	return driftLow, nil
}

// parsePipelineRemote decodes the body of GET /api/sites/{id}/pipeline — the
// response apply already fetched and used to throw away.
func parsePipelineRemote(body []byte) (pipelineRemote, error) {
	var out pipelineRemote
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("parse pipeline: %w (body=%s)", err, provisionSummarize(body))
	}
	return out, nil
}

// pipelineStepKind is the identity of a local step. The API keys steps by `name`;
// `type` is accepted in the YAML as a synonym and is what older specs use, so it
// is the fallback.
func pipelineStepKind(s pipelineStep) string {
	if k := strings.TrimSpace(s.Name); k != "" {
		return k
	}
	return strings.TrimSpace(s.Type)
}

// pipelineStepEnabled is the enabled value the apply would actually write. The
// API treats an absent `enabled` as false (mode=replace writes
// `enabled: update.Enabled != nil && *update.Enabled`), so omitting the field in
// YAML disables the step. The diff has to model that, or an apply that silently
// switches a step off would read as a NOOP.
func pipelineStepEnabled(s pipelineStep) bool {
	return s.Enabled != nil && *s.Enabled
}

// pipelineKeys assigns each step a match key of "<kind>#<occurrence>". A pipeline
// may legitimately run the same kind more than once (two quality_validation
// instances with different settings), so the kind alone is not an identity.
// Occurrence is the ordinal among steps of that kind, in pipeline order, which is
// stable under edits to any other kind.
func pipelineKeys(kinds []string) []string {
	seen := map[string]int{}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = fmt.Sprintf("%s#%d", k, seen[k])
		seen[k]++
	}
	return out
}

// pipelineLabel names a step in human-facing output: the bare kind when it is
// unique, kind + occurrence when the pipeline runs it more than once.
func pipelineLabel(key string) string {
	if strings.HasSuffix(key, "#0") {
		return strings.TrimSuffix(key, "#0")
	}
	return key
}

// diffPipeline compares the steps this apply would send against the live
// pipeline. Steps are matched on kind + occurrence; position is compared for the
// matched pairs, so a reorder shows up as a modification rather than passing
// unnoticed (array order is execution order).
func diffPipeline(local []pipelineStep, remote pipelineRemote) pipelineDiff {
	d := pipelineDiff{RemoteStepCount: len(remote.Steps), LocalStepCount: len(local)}

	localKinds := make([]string, len(local))
	for i, s := range local {
		localKinds[i] = pipelineStepKind(s)
	}
	remoteKinds := make([]string, len(remote.Steps))
	for i, s := range remote.Steps {
		remoteKinds[i] = strings.TrimSpace(s.Type)
	}
	localKeys := pipelineKeys(localKinds)
	remoteKeys := pipelineKeys(remoteKinds)

	type remoteEntry struct {
		step  pipelineStepRemote
		index int
	}
	byKey := make(map[string]remoteEntry, len(remote.Steps))
	for i, s := range remote.Steps {
		byKey[remoteKeys[i]] = remoteEntry{step: s, index: i}
	}

	wanted := make(map[string]bool, len(local))
	for i, want := range local {
		key := localKeys[i]
		wanted[key] = true
		cur, found := byKey[key]
		if !found {
			d.Steps = append(d.Steps, pipelineStepDiff{Kind: stepAdded, Key: key, Label: pipelineLabel(key)})
			continue
		}
		changes := diffPipelineStepFields(cur.step, cur.index, want, i)
		if len(changes) > 0 {
			d.Steps = append(d.Steps, pipelineStepDiff{Kind: stepModified, Key: key, Label: pipelineLabel(key), Changes: changes})
		}
	}
	for i, key := range remoteKeys {
		if !wanted[key] {
			d.Steps = append(d.Steps, pipelineStepDiff{
				Kind:  stepRemoved,
				Key:   key,
				Label: pipelineLabel(key),
				Changes: []fieldChange{{
					Path: "position", Old: fmt.Sprint(i), New: "(absent)",
				}},
			})
		}
	}
	return d
}

// diffPipelineStepFields compares one matched step. Only fields the apply would
// actually send are compared: a field the local file omits is not written, and
// the remote value for it may be a site-level default the API filled in, so
// treating its absence as drift would flag every pipeline on every run.
//
// enabled is the exception — it is always effectively sent (an absent `enabled`
// writes false), so it is always compared.
func diffPipelineStepFields(remote pipelineStepRemote, remoteIdx int, local pipelineStep, localIdx int) []fieldChange {
	var out []fieldChange

	if remoteIdx != localIdx {
		out = append(out, fieldChange{Path: "position", Old: fmt.Sprint(remoteIdx), New: fmt.Sprint(localIdx)})
	}

	localEnabled := pipelineStepEnabled(local)
	if remote.Enabled != localEnabled {
		fc := fieldChange{Path: "enabled", Old: fmt.Sprint(remote.Enabled), New: fmt.Sprint(localEnabled)}
		if remote.Enabled && !localEnabled {
			// Switching a step off stops it running: the same class of loss as
			// deleting it, so it is gated the same way. Switching one on is additive.
			fc.Reason = reasonPipelineStepDisable
		}
		out = append(out, fc)
	}

	// provider/model: compared only when the local file declares them, because
	// the API infers both from the site AI config when a step sets neither.
	if p := strings.TrimSpace(local.Provider); p != "" && p != remote.Provider {
		out = append(out, fieldChange{Path: "provider", Old: fmtScalar(remote.Provider), New: fmtScalar(p), Reason: reasonPipelineModelChange})
	}
	if m := strings.TrimSpace(local.Model); m != "" && m != remote.Model {
		out = append(out, fieldChange{Path: "model", Old: fmtScalar(remote.Model), New: fmtScalar(m), Reason: reasonPipelineModelChange})
	}

	out = append(out, diffPipelineSettings(remote.Settings, local.Settings)...)
	return out
}

// diffPipelineSettings compares a step's settings per leaf, with dotted paths
// (settings.max_links, settings.deployment.bucket) so a threshold change reports
// as one line rather than "settings changed".
//
// Asymmetric on purpose, for the reasons in the file header: a key the remote has
// and the local does not is only reported when it is not one of the site-level AI
// defaults the API merges in; a key the local has and the remote does not is
// skipped when the API is known to redact it.
func diffPipelineSettings(remote, local map[string]interface{}) []fieldChange {
	localNorm, _ := convertYAMLValue(local).(map[string]interface{})
	if localNorm == nil {
		localNorm = map[string]interface{}{}
	}
	if remote == nil {
		remote = map[string]interface{}{}
	}

	keys := map[string]bool{}
	for k := range remote {
		keys[k] = true
	}
	for k := range localNorm {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []fieldChange
	for _, k := range sorted {
		rv, rhas := remote[k]
		lv, lhas := localNorm[k]
		path := "settings." + k
		switch {
		case !lhas:
			if pipelineInheritedSettingKeys[k] {
				continue // site-level AI default, not something the local file dropped
			}
			out = append(out, fieldChange{Path: path, Old: fmtJSON(rv), New: "(absent)"})
		case !rhas:
			if pipelineRedactedSettingKeys[k] {
				continue // the API never returns it; comparing it drifts forever
			}
			out = append(out, fieldChange{Path: path, Old: "(absent)", New: fmtJSON(lv)})
		default:
			out = append(out, diffJSONValues(path, rv, lv)...)
		}
	}
	// A model/provider key nested in settings (settings.model, or the provider_id
	// an enrichment step calls) re-points the step just as the top-level field
	// does, so it is gated the same way.
	for i := range out {
		if out[i].Reason == "" && pipelineIsRepointPath(out[i].Path) {
			out[i].Reason = reasonPipelineModelChange
		}
	}
	return out
}

// pipelineIsRepointPath reports whether a settings path names a field that
// re-points which model or provider the step runs against. It extends the
// playbook gate's rule (isAIModelPath: a trailing `model`/`provider` at any
// depth) with provider_id, which is how a pipeline step addresses a custom
// provider. Kept local rather than added to the shared key set, so the playbook
// gate's classification does not shift underneath it.
func pipelineIsRepointPath(path string) bool {
	if isAIModelPath(path) {
		return true
	}
	idx := strings.LastIndex(path, ".")
	return idx >= 0 && path[idx+1:] == "provider_id"
}
