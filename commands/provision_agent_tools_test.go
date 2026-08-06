package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type agentToolsTestServer struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (s *agentToolsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "want PATCH", http.StatusMethodNotAllowed)
			return
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bodyBytes, &body)
		s.mu.Lock()
		s.calls = append(s.calls, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": 12})
	})
}

func TestApplyAgentTools_HappyPath_SendsBothFields(t *testing.T) {
	srv := &agentToolsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `agent_tool_policy:
  - web_search
  - call_studio_api
  - notify_human
studio_api_allowlist:
  - method: GET
    path_pattern: /presentation-templates
  - method: POST
    path_pattern: /presentations
`
	if err := os.WriteFile(filepath.Join(dir, "agent-tools.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyAgentTools(c, dir, 12); err != nil {
		t.Fatalf("applyAgentTools: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PATCH call, got %d", len(srv.calls))
	}
	policy, _ := srv.calls[0]["agent_tool_policy"].([]any)
	if len(policy) != 3 {
		t.Errorf("agent_tool_policy = %v, want 3 entries", policy)
	}
	allowlist, _ := srv.calls[0]["studio_api_allowlist"].([]any)
	if len(allowlist) != 2 {
		t.Errorf("studio_api_allowlist = %v, want 2 entries", allowlist)
	}
}

// TestApplyAgentTools_OnlyPolicySet_DoesNotSendAllowlist confirms the
// nil-vs-empty-slice contract: omitting studio_api_allowlist from the YAML
// entirely must not send that key at all (server-side "leave unchanged"),
// not send it as an empty list (server-side "explicitly clear").
func TestApplyAgentTools_OnlyPolicySet_DoesNotSendAllowlist(t *testing.T) {
	srv := &agentToolsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `agent_tool_policy:
  - web_search
`
	if err := os.WriteFile(filepath.Join(dir, "agent-tools.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyAgentTools(c, dir, 12); err != nil {
		t.Fatalf("applyAgentTools: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PATCH call, got %d", len(srv.calls))
	}
	if _, present := srv.calls[0]["studio_api_allowlist"]; present {
		t.Errorf("studio_api_allowlist key present = %v, want absent (leave unchanged), got %+v", present, srv.calls[0])
	}
	if _, present := srv.calls[0]["agent_tool_policy"]; !present {
		t.Error("agent_tool_policy key missing, want present")
	}
}

func TestApplyAgentTools_MissingFileIsNoop(t *testing.T) {
	srv := &agentToolsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir() // no agent-tools.yaml
	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyAgentTools(c, dir, 12); err != nil {
		t.Errorf("missing file should be no-op, got error: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("expected zero API calls, got %d", len(srv.calls))
	}
}

func TestApplyAgentTools_DryRunMakesNoWrite(t *testing.T) {
	srv := &agentToolsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `agent_tool_policy: [web_search]`
	if err := os.WriteFile(filepath.Join(dir, "agent-tools.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", true) // dryRun=true
	if err := applyAgentTools(c, dir, 12); err != nil {
		t.Fatalf("applyAgentTools dryRun: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("dryRun should not hit the server; got %+v", srv.calls)
	}
}

func TestApplyAgentTools_EmptyListExplicitlyClears(t *testing.T) {
	srv := &agentToolsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	yaml := `agent_tool_policy: []`
	if err := os.WriteFile(filepath.Join(dir, "agent-tools.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyAgentTools(c, dir, 12); err != nil {
		t.Fatalf("applyAgentTools: %v", err)
	}
	if len(srv.calls) != 1 {
		t.Fatalf("want 1 PATCH call, got %d", len(srv.calls))
	}
	policy, present := srv.calls[0]["agent_tool_policy"]
	if !present {
		t.Fatal("agent_tool_policy key missing, want present (explicit empty list)")
	}
	if arr, ok := policy.([]any); !ok || len(arr) != 0 {
		t.Errorf("agent_tool_policy = %v, want an empty list", policy)
	}
}
