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

// pipelineWrite is one mutating call the apply issued. The tests assert on these
// rather than on stdout: what matters is whether provision would have rewritten
// the live pipeline.
type pipelineWrite struct {
	Method string
	Path   string
	Body   string
}

// pipelineTestServer is a minimal Studio stand-in serving one site (id=9) and its
// pipeline. Every non-GET is recorded; with failOnWrite set, any write fails the
// test at the moment it is issued — that is how "NOOP means zero writes" is
// asserted rather than merely implied.
type pipelineTestServer struct {
	t           *testing.T
	mu          sync.Mutex
	steps       []map[string]any
	versions    []map[string]int
	writes      []pipelineWrite
	failOnWrite bool
}

const testPipelineSiteID = 9

func (s *pipelineTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.writes = append(s.writes, pipelineWrite{Method: r.Method, Path: r.URL.Path, Body: string(body)})
			fail := s.failOnWrite
			s.mu.Unlock()
			if fail {
				s.t.Errorf("unexpected write: %s %s body=%s", r.Method, r.URL.Path, string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":"ok"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/sites/by-site-id/testsite":
			org := uint(12)
			_ = json.NewEncoder(w).Encode(siteRecord{ID: testPipelineSiteID, SiteID: "testsite", Name: "Test site", OrganizationID: &org})
		case "/api/sites/9/pipeline":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"steps":                 s.steps,
				"topic_discovery_steps": []any{},
			})
		case "/api/sites/9/pipeline/versions":
			_ = json.NewEncoder(w).Encode(s.versions)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *pipelineTestServer) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// baseRemoteSteps is the live pipeline that basePipelineYAML is an exact,
// in-sync snapshot of.
//
// It deliberately carries the two things the API adds on read and the local file
// never declares: `temperature` / `max_tokens` merged down from the site's AI
// config, and a `provider` inferred from the model name. If the diff compared
// those, every apply of an unchanged file would report drift.
func baseRemoteSteps() []map[string]any {
	return []map[string]any{
		{
			"type":    "generation",
			"enabled": true,
			"settings": map[string]any{
				"prompt_prefix": "Write clearly.",
				"temperature":   0.7,
			},
		},
		{
			"type":     "safety_validation",
			"enabled":  true,
			"model":    "model-a",
			"provider": "anthropic",
			"settings": map[string]any{
				"threshold":  60,
				"max_tokens": 4096,
			},
		},
	}
}

const basePipelineYAML = `steps:
  - name: generation
    enabled: true
    settings:
      prompt_prefix: "Write clearly."

  - name: safety_validation
    enabled: true
    model: model-a
    settings:
      threshold: 60
`

// newPipelineFixture stands up the fake Studio, writes site.yaml + pipeline.yaml
// into a temp sites/testsite/ dir, and returns the server, the dir applySites
// takes, and a live client.
func newPipelineFixture(t *testing.T, srv *pipelineTestServer, pipelineYAML string) (*httptest.Server, string, *provisionClient) {
	t.Helper()
	srv.t = t
	if srv.versions == nil {
		srv.versions = []map[string]int{{"version": 4}}
	}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	siteDir := filepath.Join(dir, "sites", "testsite")
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "site.yaml"), []byte("site_id: testsite\n"), 0o644); err != nil {
		t.Fatalf("write site.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "pipeline.yaml"), []byte(pipelineYAML), 0o644); err != nil {
		t.Fatalf("write pipeline.yaml: %v", err)
	}
	return ts, dir, newProvisionClient(ts.URL, "test-key", false)
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestPipelineApply_InSyncIsNoopWithZeroWrites — the core regression. The old
// implementation PUT the whole pipeline unconditionally (mode=replace), so it
// rewrote live config even for a file that already matched. failOnWrite makes the
// assertion real: any write fails the test the moment it is issued.
func TestPipelineApply_InSyncIsNoopWithZeroWrites(t *testing.T) {
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	_, dir, c := newPipelineFixture(t, srv, basePipelineYAML)

	if err := applySites(c, dir, 12, false); err != nil {
		t.Fatalf("applySites: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("in-sync apply must be a NOOP; got %d write(s)", n)
	}
}

// TestPipelineApply_RemovedStepIsHighDriftAndRefused — the failure this gate
// exists for. mode=replace turns a step missing from the local file into a
// deletion, so a stale pipeline.yaml silently strips live steps.
func TestPipelineApply_RemovedStepIsHighDriftAndRefused(t *testing.T) {
	yaml := basePipelineYAML[:strings.Index(basePipelineYAML, "  - name: safety_validation")]
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	err := applySites(c, dir, 12, false)
	if err == nil {
		t.Fatal("a removed step must be refused as HIGH drift without --allow-drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must write nothing; got %d write(s)", n)
	}
}

// TestPipelineApply_RemovedStepAppliesWithAllowDrift — with the opt-in the
// replace lands, and the payload really does drop the step (that is the whole
// danger, spelled out in an assertion).
func TestPipelineApply_RemovedStepAppliesWithAllowDrift(t *testing.T) {
	yaml := basePipelineYAML[:strings.Index(basePipelineYAML, "  - name: safety_validation")]
	srv := &pipelineTestServer{steps: baseRemoteSteps()}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	if err := applySites(c, dir, 12, true); err != nil {
		t.Fatalf("applySites --allow-drift: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want exactly 1 write (the pipeline PUT), got %d: %+v", len(srv.writes), srv.writes)
	}
	w := srv.writes[0]
	if w.Method != "PUT" || w.Path != "/api/sites/9/pipeline" {
		t.Errorf("want PUT on the site pipeline, got %s %s", w.Method, w.Path)
	}
	if !strings.Contains(w.Body, `"mode":"replace"`) {
		t.Errorf("payload should be a replace, got %s", w.Body)
	}
	if strings.Contains(w.Body, "safety_validation") {
		t.Errorf("the removed step must not be in the payload (that is what deletes it), got %s", w.Body)
	}
}

// TestPipelineApply_StepTypeChangeIsHighDriftAndRefused — a step's kind is its
// identity, so changing it swaps one live step for another: the old one is
// deleted by the replace.
func TestPipelineApply_StepTypeChangeIsHighDriftAndRefused(t *testing.T) {
	yaml := strings.Replace(basePipelineYAML, "- name: safety_validation", "- name: seo_optimization", 1)
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	err := applySites(c, dir, 12, false)
	if err == nil {
		t.Fatal("a step type change must be refused as HIGH drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must write nothing; got %d write(s)", n)
	}
}

// TestPipelineApply_ModelChangeIsHighDriftAndRefused — the pipeline keeps
// running, but against a different model. Nothing about the apply looks unusual,
// which is exactly why it is gated.
func TestPipelineApply_ModelChangeIsHighDriftAndRefused(t *testing.T) {
	yaml := strings.Replace(basePipelineYAML, "model: model-a", "model: model-b", 1)
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	err := applySites(c, dir, 12, false)
	if err == nil {
		t.Fatal("an AI model change must be refused as HIGH drift")
	}
	if !strings.Contains(err.Error(), "HIGH drift") {
		t.Errorf("error should name the drift gate, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must write nothing; got %d write(s)", n)
	}
}

// TestPipelineApply_DisablingAStepIsHighDriftAndRefused — a disabled step stops
// running, which is the same loss of live behaviour as deleting it. Worth its own
// test because the API treats an *absent* `enabled` as false, so this is easy to
// trigger by accident.
func TestPipelineApply_DisablingAStepIsHighDriftAndRefused(t *testing.T) {
	yaml := strings.Replace(basePipelineYAML, "  - name: safety_validation\n    enabled: true", "  - name: safety_validation\n    enabled: false", 1)
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	err := applySites(c, dir, 12, false)
	if err == nil {
		t.Fatal("disabling a live step must be refused as HIGH drift")
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("a refused apply must write nothing; got %d write(s)", n)
	}
}

// TestPipelineApply_ThresholdOnlyChangeIsLowAndApplies — a targeted settings edit
// is not drift worth blocking. It applies with no opt-in, and writes once.
func TestPipelineApply_ThresholdOnlyChangeIsLowAndApplies(t *testing.T) {
	yaml := strings.Replace(basePipelineYAML, "threshold: 60", "threshold: 70", 1)
	srv := &pipelineTestServer{steps: baseRemoteSteps()}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	if err := applySites(c, dir, 12, false); err != nil {
		t.Fatalf("a threshold-only change should apply, got: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want exactly 1 write, got %d: %+v", len(srv.writes), srv.writes)
	}
	if !strings.Contains(srv.writes[0].Body, "70") {
		t.Errorf("payload should carry the new threshold, got %s", srv.writes[0].Body)
	}
}

// TestPipelineApply_PromptOnlyChangeIsLowAndApplies — same, for the other kind of
// everyday edit.
func TestPipelineApply_PromptOnlyChangeIsLowAndApplies(t *testing.T) {
	yaml := strings.Replace(basePipelineYAML, `prompt_prefix: "Write clearly."`, `prompt_prefix: "Write clearly and briefly."`, 1)
	srv := &pipelineTestServer{steps: baseRemoteSteps()}
	_, dir, c := newPipelineFixture(t, srv, yaml)

	if err := applySites(c, dir, 12, false); err != nil {
		t.Fatalf("a prompt-only change should apply, got: %v", err)
	}
	if len(srv.writes) != 1 {
		t.Fatalf("want exactly 1 write, got %d: %+v", len(srv.writes), srv.writes)
	}
}

// TestPipelineApply_DryRunWithHighDriftWarnsAndExitsZero — a dry-run must always
// be safe to run: it reports the HIGH drift, never refuses, never writes.
func TestPipelineApply_DryRunWithHighDriftWarnsAndExitsZero(t *testing.T) {
	yaml := basePipelineYAML[:strings.Index(basePipelineYAML, "  - name: safety_validation")]
	srv := &pipelineTestServer{steps: baseRemoteSteps(), failOnWrite: true}
	ts, dir, _ := newPipelineFixture(t, srv, yaml)
	c := newProvisionClient(ts.URL, "test-key", true) // dryRun

	if err := applySites(c, dir, 12, false); err != nil {
		t.Fatalf("dry-run must not refuse, got: %v", err)
	}
	if n := srv.writeCount(); n != 0 {
		t.Errorf("dry-run must not write; got %d write(s)", n)
	}
}

// ─── Diff-level behaviour ────────────────────────────────────────────────────

// TestDiffPipeline_ReorderIsDetectedAndReported — array order is execution order,
// so swapping two steps changes behaviour even though every step survives. It
// must show up as a position change on both steps, not as a NOOP.
func TestDiffPipeline_ReorderIsDetectedAndReported(t *testing.T) {
	enabled := true
	local := []pipelineStep{
		{Name: "safety_validation", Enabled: &enabled, Model: "model-a", Settings: map[string]any{"threshold": 60}},
		{Name: "generation", Enabled: &enabled, Settings: map[string]any{"prompt_prefix": "Write clearly."}},
	}
	remote := pipelineRemote{Steps: []pipelineStepRemote{
		{Type: "generation", Enabled: true, Settings: map[string]any{"prompt_prefix": "Write clearly.", "temperature": 0.7}},
		{Type: "safety_validation", Enabled: true, Model: "model-a", Provider: "anthropic", Settings: map[string]any{"threshold": 60, "max_tokens": 4096}},
	}}

	diff := diffPipeline(local, remote)
	if diff.empty() {
		t.Fatal("a reorder must not read as a NOOP")
	}
	moved := map[string]string{}
	for _, s := range diff.Steps {
		if s.Kind != stepModified {
			t.Fatalf("a reorder must be a modification, got %s on %q", s.Kind, s.Label)
		}
		for _, ch := range s.Changes {
			if ch.Path == "position" {
				moved[s.Label] = ch.Old + "->" + ch.New
			}
		}
	}
	if moved["generation"] != "0->1" || moved["safety_validation"] != "1->0" {
		t.Errorf("both steps should report their new position, got %v", moved)
	}
	// Reordering keeps every step and every model, so it is a targeted change.
	if sev, reasons := diff.severity(); sev != driftLow {
		t.Errorf("a two-step reorder should be LOW, got %s (%v)", sev, reasons)
	}
}

// TestDiffPipeline_RepeatedStepKindsMatchByOccurrence — a pipeline may run the
// same kind twice (two quality_validation instances with different settings).
// Keying on the kind alone would collapse them onto each other and diff the wrong
// pair; they key on kind + occurrence instead.
func TestDiffPipeline_RepeatedStepKindsMatchByOccurrence(t *testing.T) {
	enabled := true
	local := []pipelineStep{
		{Name: "quality_validation", Enabled: &enabled, Settings: map[string]any{"instance_name": "first"}},
		{Name: "quality_validation", Enabled: &enabled, Settings: map[string]any{"instance_name": "second"}},
	}
	remote := pipelineRemote{Steps: []pipelineStepRemote{
		{Type: "quality_validation", Enabled: true, Settings: map[string]any{"instance_name": "first"}},
		{Type: "quality_validation", Enabled: true, Settings: map[string]any{"instance_name": "second"}},
	}}
	if diff := diffPipeline(local, remote); !diff.empty() {
		t.Errorf("two in-sync instances of the same kind must be a NOOP, got %+v", diff.Steps)
	}

	// Dropping the second instance is still a deletion, and still HIGH.
	shorter := diffPipeline(local[:1], remote)
	if sev, _ := shorter.severity(); sev != driftHigh {
		t.Errorf("dropping the second instance must be HIGH, got %s", sev)
	}
}

// TestDiffPipeline_InheritedAndRedactedSettingsAreNotDrift — the API merges the
// site's AI defaults into every step it returns and strips secret-looking keys.
// Neither is a difference the local file created, and reporting them would flag
// drift on every apply — which trains everyone to reach for --allow-drift and
// hollows out the gate.
func TestDiffPipeline_InheritedAndRedactedSettingsAreNotDrift(t *testing.T) {
	enabled := true
	local := []pipelineStep{{
		Name:     "data_enrichment",
		Enabled:  &enabled,
		Settings: map[string]any{"endpoint_url": "https://example.invalid/x", "api_key": "s3cret"},
	}}
	remote := pipelineRemote{Steps: []pipelineStepRemote{{
		Type:    "data_enrichment",
		Enabled: true,
		// temperature/max_tokens copied down from the site AI config; api_key
		// redacted out of the response.
		Settings: map[string]any{"endpoint_url": "https://example.invalid/x", "temperature": 0.7, "max_tokens": 4096},
	}}}

	if diff := diffPipeline(local, remote); !diff.empty() {
		t.Errorf("inherited and redacted settings must not read as drift, got %+v", diff.Steps[0].Changes)
	}
}

// TestDiffPipeline_ProviderIDChangeIsHighDrift — an enrichment step addresses its
// custom provider through settings.provider_id. Re-pointing it is the same class
// of change as swapping a model.
func TestDiffPipeline_ProviderIDChangeIsHighDrift(t *testing.T) {
	enabled := true
	local := []pipelineStep{{
		Name: "data_enrichment", Enabled: &enabled,
		Settings: map[string]any{"provider_id": 8},
	}}
	remote := pipelineRemote{Steps: []pipelineStepRemote{{
		Type: "data_enrichment", Enabled: true,
		Settings: map[string]any{"provider_id": 3},
	}}}

	diff := diffPipeline(local, remote)
	sev, reasons := diff.severity()
	if sev != driftHigh {
		t.Fatalf("a provider_id re-point must be HIGH, got %s", sev)
	}
	if !strings.Contains(strings.Join(reasons, "; "), reasonPipelineModelChange) {
		t.Errorf("the reason should name the re-point, got %v", reasons)
	}
}
