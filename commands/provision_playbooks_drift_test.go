package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── Fake Studio ─────────────────────────────────────────────────────────────

// playbookWrite is one mutating call the apply issued. The tests assert on these
// rather than on stdout: what matters is which rows provision would have touched.
type playbookWrite struct {
	Method string
	Path   string
	Body   string
}

// playbookTestServer is a minimal Studio stand-in serving one playbook (id=7).
// Every non-GET is recorded. When failOnWrite is set, any write fails the test —
// that is how "NOOP means zero writes" is asserted rather than merely implied.
type playbookTestServer struct {
	t           *testing.T
	mu          sync.Mutex
	detail      playbookDetailRemote
	steps       []provisionPlaybookStepRemote
	writes      []playbookWrite
	failOnWrite bool
}

const testPlaybookID = 7

func (s *playbookTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.writes = append(s.writes, playbookWrite{Method: r.Method, Path: r.URL.Path, Body: string(body)})
			fail := s.failOnWrite
			s.mu.Unlock()
			if fail {
				s.t.Errorf("unexpected write: %s %s body=%s", r.Method, r.URL.Path, string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/playbooks/":
			_ = json.NewEncoder(w).Encode([]provisionPlaybookListItem{
				{ID: testPlaybookID, Name: s.detail.Name, Slug: s.detail.Slug},
			})
		case "/api/playbooks/7":
			_ = json.NewEncoder(w).Encode(s.detail)
		case "/api/playbooks/7/steps":
			_ = json.NewEncoder(w).Encode(s.steps)
		case "/api/playbooks/7/versions":
			_ = json.NewEncoder(w).Encode([]map[string]int{{"version": 3}})
		case "/api/credentials/":
			_ = json.NewEncoder(w).Encode([]provisionCredentialListItem{})
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *playbookTestServer) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// baseDetail is the live playbook the local YAML in baseYAML is an exact,
// in-sync snapshot of.
func baseDetail() playbookDetailRemote {
	return playbookDetailRemote{
		Name:             "Weekly digest",
		Slug:             "weekly-digest",
		Description:      "Summarises the week",
		TriggerType:      "manual",
		ScheduleTimezone: "UTC",
		OutputKey:        "summary",
		Enabled:          true,
		AgentTriggerable: false,
	}
}

func baseSteps() []provisionPlaybookStepRemote {
	return []provisionPlaybookStepRemote{
		{ID: 11, Name: "Collect", StepType: "llm_extract", Position: 0, Config: `{"model":"model-a"}`, OutputKey: "items", Enabled: true},
		{ID: 12, Name: "Publish", StepType: "http_post", Position: 1, Config: `{"url":"https://example.invalid/hook"}`, OutputKey: "posted", Enabled: true},
	}
}

const baseYAML = `name: Weekly digest
slug: weekly-digest
description: Summarises the week
trigger_type: manual
output_key: summary
schedule_timezone: UTC
enabled: true
agent_triggerable: false
steps:
  - name: Collect
    step_type: llm_extract
    output_key: items
    enabled: true
    config:
      model: model-a
  - name: Publish
    step_type: http_post
    output_key: posted
    enabled: true
    config:
      url: https://example.invalid/hook
`

// newPlaybookFixture stands up the fake Studio, writes the YAML to a temp
// studio/playbooks/ dir, and returns the server, the dir and a live client.
func newPlaybookFixture(t *testing.T, srv *playbookTestServer, yaml string) (*httptest.Server, string, *provisionClient) {
	t.Helper()
	srv.t = t
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	pd := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "weekly-digest.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return ts, dir, newProvisionClient(ts.URL, "test-key", false)
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestPlaybookApply_InSyncIsNoopWithZeroWrites — the core regression. The old
// implementation PUT the playbook and every step unconditionally; an apply of a
// file that already matches live must now touch nothing at all. failOnWrite makes
// the assertion real: any write fails the test at the moment it is issued.
func TestPlaybookApply_InSyncIsNoopWithZeroWrites(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps(), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, baseYAML)

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("applyPlaybooks: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("in-sync apply must be a NOOP; got %d write(s)", n)
	}
}

// TestPlaybookApply_AIModelChangeIsHighDriftAndRefused — swapping the model a
// step runs against silently re-points production behaviour, so it is HIGH and
// must not apply without the operator opting in.
func TestPlaybookApply_AIModelChangeIsHighDriftAndRefused(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps(), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, strings.Replace(baseYAML, "model: model-a", "model: model-b", 1))

	err := applyPlaybooks(c, dir, 12, false)
	if err == nil {
		t.Fatal("model change must be refused as HIGH drift without --allow-drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must write nothing; got %d write(s)", n)
	}
}

// TestPlaybookApply_AIModelChangeAppliesWithAllowDrift — with the opt-in, the
// change lands, and only the step that actually changed is written: no playbook
// PUT (no field drift), no touch of the unchanged step.
func TestPlaybookApply_AIModelChangeAppliesWithAllowDrift(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps()}
	_, dir, c := newPlaybookFixture(t, srv, strings.Replace(baseYAML, "model: model-a", "model: model-b", 1))

	if err := applyPlaybooks(c, dir, 12, true); err != nil {
		t.Fatalf("applyPlaybooks --allow-drift: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want exactly 1 write (the changed step), got %d: %+v", len(srv.writes), srv.writes)
	}
	w := srv.writes[0]
	if w.Method != "PUT" || w.Path != "/api/playbooks/7/steps/11" {
		t.Errorf("want PUT on step 11, got %s %s", w.Method, w.Path)
	}
	if !strings.Contains(w.Body, "model-b") {
		t.Errorf("payload should carry the new model, got %s", w.Body)
	}
}

// TestPlaybookApply_StepDeletionIsHighDriftAndRefused — a step present live but
// absent from the YAML is DELETEd. If the local file is stale that destroys live
// configuration, so it is refused by default.
func TestPlaybookApply_StepDeletionIsHighDriftAndRefused(t *testing.T) {
	// YAML with the Publish step dropped.
	yaml := baseYAML[:strings.Index(baseYAML, "  - name: Publish")]
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps(), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, yaml)

	err := applyPlaybooks(c, dir, 12, false)
	if err == nil {
		t.Fatal("step deletion must be refused as HIGH drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must issue no DELETE; got %d write(s)", n)
	}
}

// TestPlaybookApply_DescriptionOnlyChangeIsLowAndApplies — a cosmetic edit is not
// drift worth blocking. It applies without an opt-in, and writes only the
// playbook row (no step touched).
func TestPlaybookApply_DescriptionOnlyChangeIsLowAndApplies(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps()}
	yaml := strings.Replace(baseYAML, "description: Summarises the week", "description: Summarises the week, briefly", 1)
	_, dir, c := newPlaybookFixture(t, srv, yaml)

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("description-only change should apply, got: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want exactly 1 write (the playbook row), got %d: %+v", len(srv.writes), srv.writes)
	}
	if srv.writes[0].Method != "PUT" || srv.writes[0].Path != "/api/playbooks/7" {
		t.Errorf("want PUT on the playbook, got %s %s", srv.writes[0].Method, srv.writes[0].Path)
	}
}

// TestPlaybookApply_DryRunWithHighDriftWarnsAndExitsZero — a dry-run must always
// be safe to run: it reports the HIGH drift but never refuses and never writes.
func TestPlaybookApply_DryRunWithHighDriftWarnsAndExitsZero(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: baseSteps(), failOnWrite: true}
	ts, dir, _ := newPlaybookFixture(t, srv, strings.Replace(baseYAML, "model: model-a", "model: model-b", 1))
	c := newProvisionClient(ts.URL, "test-key", true) // dryRun

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("dry-run must not refuse, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("dry-run must not write; got %d write(s)", n)
	}
}

// ─── error_policy ────────────────────────────────────────────────────────────

const errorPolicyYAML = `name: Weekly digest
slug: weekly-digest
description: Summarises the week
trigger_type: manual
output_key: summary
schedule_timezone: UTC
enabled: true
agent_triggerable: false
steps:
  - name: Collect
    step_type: llm_extract
    output_key: items
    enabled: true
    config:
      model: model-a
    error_policy:
      action: skip
`

func errorPolicySteps(action string) []provisionPlaybookStepRemote {
	return []provisionPlaybookStepRemote{
		{ID: 11, Name: "Collect", StepType: "llm_extract", Position: 0,
			Config: `{"model":"model-a"}`, OutputKey: "items", Enabled: true,
			ErrorPolicy: &provisionErrorPolicy{Action: action}},
	}
}

// TestPlaybookApply_ErrorPolicyRoundTripsToNoop — error_policy must load from the
// YAML and compare against the live value. If it were not modelled, this apply
// would look identical to one that strips the policy.
func TestPlaybookApply_ErrorPolicyRoundTripsToNoop(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: errorPolicySteps(provisionErrorPolicySkip), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, errorPolicyYAML)

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("applyPlaybooks: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("matching error_policy must be a NOOP; got %d write(s)", n)
	}
}

// TestPlaybookApply_ErrorPolicyActionChangeIsHighDrift — the action decides
// whether a step failure aborts the run. Changing it rewrites production failure
// behaviour, so it is HIGH.
func TestPlaybookApply_ErrorPolicyActionChangeIsHighDrift(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: errorPolicySteps(provisionErrorPolicyAbort), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, errorPolicyYAML) // local says skip, live says abort

	err := applyPlaybooks(c, dir, 12, false)
	if err == nil {
		t.Fatal("error_policy.action change must be refused as HIGH drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
}

// TestPlaybookApply_ErrorPolicySentInStepPayload — the regression that motivated
// modelling this field at all: provision sends a full step snapshot, so a payload
// with no error_policy key clears the live policy. The key must always be present
// (explicitly null when the YAML has none).
func TestPlaybookApply_ErrorPolicySentInStepPayload(t *testing.T) {
	// Live step has no policy; local YAML adds one → the step is written.
	steps := errorPolicySteps("")
	steps[0].ErrorPolicy = nil
	srv := &playbookTestServer{detail: baseDetail(), steps: steps}
	_, dir, c := newPlaybookFixture(t, srv, errorPolicyYAML)

	if err := applyPlaybooks(c, dir, 12, true); err != nil {
		t.Fatalf("applyPlaybooks --allow-drift: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want 1 step write, got %d: %+v", len(srv.writes), srv.writes)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(srv.writes[0].Body), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	ep, present := payload["error_policy"]
	if !present {
		t.Fatal("step payload must always carry the error_policy key")
	}
	if string(ep) != `{"action":"skip"}` {
		t.Errorf("error_policy = %s, want {\"action\":\"skip\"}", ep)
	}
}

// TestDesiredStepPayload_NullsAbsentErrorPolicy — the other half of the wire
// contract: a step with no policy in the YAML sends an explicit null, which is how
// the server is told to clear one.
func TestDesiredStepPayload_NullsAbsentErrorPolicy(t *testing.T) {
	s := desiredStep{Name: "Collect", StepType: "llm_extract", ConfigJSON: "{}", OutputKey: "out", Enabled: true}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(s.payload(), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	ep, present := payload["error_policy"]
	if !present || string(ep) != "null" {
		t.Errorf("error_policy = %s (present=%v), want explicit null", ep, present)
	}
}

// ─── Position calibration ────────────────────────────────────────────────────

// TestPlaybookApply_PositionOnlyChangeIsNotHighDrift — remote positions are often
// sparse (a step was deleted and the rest never renumbered) while provision derives
// contiguous ones. Renumbering 5,9 to 0,1 preserves the exact order, so it is not a
// behaviour change and must not count towards drift severity. Without this
// exclusion both steps count as "touched" (2 of 2 remote steps, > 50%) and the
// playbook flags HIGH — which would train operators to pass --allow-drift
// reflexively and hollow out the gate.
func TestPlaybookApply_PositionOnlyChangeIsNotHighDrift(t *testing.T) {
	steps := baseSteps()
	steps[0].Position = 5
	steps[1].Position = 9
	srv := &playbookTestServer{detail: baseDetail(), steps: steps}
	_, dir, c := newPlaybookFixture(t, srv, baseYAML)

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("position-only renumbering must not be HIGH drift, got: %v", err)
	}
	// The renumbering is still written — severity ignores it, apply does not.
	if len(srv.writes) != 2 {
		t.Fatalf("want 2 step writes (the renumbering), got %d: %+v", len(srv.writes), srv.writes)
	}
	for _, w := range srv.writes {
		if w.Method != "PUT" || !strings.HasPrefix(w.Path, "/api/playbooks/7/steps/") {
			t.Errorf("want step PUTs, got %s %s", w.Method, w.Path)
		}
	}
}

// ─── Nameless steps ──────────────────────────────────────────────────────────

const namelessYAML = `name: Weekly digest
slug: weekly-digest
description: Summarises the week
trigger_type: manual
output_key: summary
schedule_timezone: UTC
enabled: true
agent_triggerable: false
steps:
  - name: ""
    step_type: llm_extract
    output_key: items
    enabled: true
    config:
      model: model-a
  - name: ""
    step_type: http_post
    output_key: posted
    enabled: true
    config:
      url: https://example.invalid/hook
`

func namelessSteps() []provisionPlaybookStepRemote {
	return []provisionPlaybookStepRemote{
		{ID: 11, Name: "", StepType: "llm_extract", Position: 0, Config: `{"model":"model-a"}`, OutputKey: "items", Enabled: true},
		{ID: 12, Name: "", StepType: "http_post", Position: 1, Config: `{"url":"https://example.invalid/hook"}`, OutputKey: "posted", Enabled: true},
	}
}

// TestPlaybookApply_NamelessStepsRoundTripToNoop — steps with empty names exist in
// production because the UI never required one. Matching steps by name collapses
// them all onto "", which used to abort the whole playbook with "duplicate step
// name" — those playbooks could not be provisioned at all. They now key on their
// ordinal among the nameless steps, so an in-sync playbook is a NOOP.
func TestPlaybookApply_NamelessStepsRoundTripToNoop(t *testing.T) {
	srv := &playbookTestServer{detail: baseDetail(), steps: namelessSteps(), failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, namelessYAML)

	if err := applyPlaybooks(c, dir, 12, false); err != nil {
		t.Fatalf("nameless steps must provision cleanly, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("in-sync nameless playbook must be a NOOP; got %d write(s)", n)
	}
}

// TestPlaybookApply_DeletedNamelessStepIsHighDrift — dropping a nameless step from
// the YAML still destroys a live step, so it is gated like any other deletion.
func TestPlaybookApply_DeletedNamelessStepIsHighDrift(t *testing.T) {
	steps := namelessSteps()
	steps = append(steps, provisionPlaybookStepRemote{
		ID: 13, Name: "", StepType: "http_post", Position: 2,
		Config: `{"url":"https://example.invalid/second"}`, OutputKey: "also", Enabled: true,
	})
	srv := &playbookTestServer{detail: baseDetail(), steps: steps, failOnWrite: true}
	_, dir, c := newPlaybookFixture(t, srv, namelessYAML) // only 2 of the 3 nameless steps

	err := applyPlaybooks(c, dir, 12, false)
	if err == nil {
		t.Fatal("deleting a nameless step must be refused as HIGH drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must issue no DELETE; got %d write(s)", n)
	}
}

// TestStepKeyOf_NamelessOrdinalIsStableAcrossNamedSteps — the nameless ordinal
// counts only the other nameless steps. If it counted every step, renaming or
// inserting a named step would renumber the nameless ones out from under
// themselves and turn a NOOP into a delete-and-recreate.
func TestStepKeyOf_NamelessOrdinalIsStableAcrossNamedSteps(t *testing.T) {
	withoutNamed := []desiredStep{{Name: ""}, {Name: ""}}
	withNamed := []desiredStep{{Name: "Intro"}, {Name: ""}, {Name: "Middle"}, {Name: ""}}

	a := desiredStepKeys(withoutNamed)
	b := desiredStepKeys(withNamed)
	if a[0] != "unnamed#0" || a[1] != "unnamed#1" {
		t.Fatalf("nameless keys = %v, want unnamed#0, unnamed#1", a)
	}
	if b[1] != "unnamed#0" || b[3] != "unnamed#1" {
		t.Errorf("named steps must not shift the nameless ordinals: got %v", b)
	}
}
