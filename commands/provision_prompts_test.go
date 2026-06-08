package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// promptsTestServer captures every PUT /api/organizations/{id}/prompts/{name}
// call and returns a fake upsert response. Lets us assert exactly which
// names + bodies were pushed without standing up a real Studio.
type promptsTestServer struct {
	mu    sync.Mutex
	calls []promptTestCall
}

type promptTestCall struct {
	OrgID int
	Name  string
	Body  string
}

func (s *promptsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /api/organizations/{id}/prompts/{name}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		// expect ["api", "organizations", "{id}", "prompts", "{name}"]
		if len(parts) != 5 || parts[0] != "api" || parts[1] != "organizations" || parts[3] != "prompts" {
			http.NotFound(w, r)
			return
		}
		orgID := parts[2]
		name := parts[4]
		bodyBytes, _ := io.ReadAll(r.Body)
		var req struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(bodyBytes, &req)

		id, _ := strconv.Atoi(orgID)
		s.mu.Lock()
		s.calls = append(s.calls, promptTestCall{OrgID: id, Name: name, Body: req.Body})
		s.mu.Unlock()

		resp := promptUpsertResponse{
			ID: 42, OrganizationID: 1, Name: name, Body: req.Body,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}

// TestApplyPrompts_HappyPath — drop two valid .txt files in studio/prompts/,
// run applyPrompts, assert the server received exactly those two PUTs with
// the correct names + bodies.
func TestApplyPrompts_HappyPath(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "linkedin_robin_content_guidelines.txt"), []byte("linkedin body"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "blog_content_guidelines.txt"), []byte("blog body"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 12); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}

	if len(srv.calls) != 2 {
		t.Fatalf("want 2 PUT calls, got %d: %+v", len(srv.calls), srv.calls)
	}
	// Order is alphabetical — blog before linkedin.
	want := []promptTestCall{
		{OrgID: 12, Name: "blog_content_guidelines", Body: "blog body"},
		{OrgID: 12, Name: "linkedin_robin_content_guidelines", Body: "linkedin body"},
	}
	for i, w := range want {
		if srv.calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, srv.calls[i], w)
		}
	}
}

// TestApplyPrompts_MissingDirIsNoop — no studio/prompts/ directory means
// nothing to provision. Must return nil (and make no API calls), matching
// the opt-in behavior of every other provision subcommand.
func TestApplyPrompts_MissingDirIsNoop(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir() // no prompts/ subdir
	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 12); err != nil {
		t.Errorf("missing dir should be no-op, got error: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("expected zero API calls, got %d", len(srv.calls))
	}
}

// TestApplyPrompts_SkipsEmptyAndNonTxt — empty .txt files and non-.txt
// files are skipped. Empty .txt counts as a warning (someone probably
// meant to remove the file rather than blank it). Non-.txt files are
// just ignored.
func TestApplyPrompts_SkipsEmptyAndNonTxt(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "prompts")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "good.txt"), []byte("ok"), 0o644)
	_ = os.WriteFile(filepath.Join(pd, "empty.txt"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(pd, "README.md"), []byte("docs"), 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 12); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if len(srv.calls) != 1 || srv.calls[0].Name != "good" {
		t.Errorf("expected single 'good' PUT, got %+v", srv.calls)
	}
	if c.WarningCount() == 0 {
		t.Errorf("expected at least 1 warning (for empty.txt), got 0")
	}
}

// TestApplyPrompts_RejectsInvalidName — filenames that produce names not
// matching the slug regex (e.g. starting with a hyphen, containing dots)
// are skipped with a warning. Mirrors the server-side validation so we
// fail locally with a clear error instead of with a 400.
func TestApplyPrompts_RejectsInvalidName(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "prompts")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "-bad-leading-hyphen.txt"), []byte("body"), 0o644)
	_ = os.WriteFile(filepath.Join(pd, "good_name.txt"), []byte("body"), 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 12); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if len(srv.calls) != 1 || srv.calls[0].Name != "good_name" {
		t.Errorf("expected only good_name push, got %+v", srv.calls)
	}
}

// TestApplyPrompts_DryRunMakesNoWrite — dry-run mode goes through the same
// code path but writeWithHeaders short-circuits to a 200 stub, so no real
// HTTP call lands on the test server. Mirrors the diff/apply pattern.
func TestApplyPrompts_DryRunMakesNoWrite(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "prompts")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "demo.txt"), []byte("body"), 0o644)

	c := newProvisionClient(ts.URL, "test-key", true) // dryRun=true
	if err := applyPrompts(c, dir, 12); err != nil {
		t.Fatalf("applyPrompts dryRun: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("dryRun should not hit the server; got %+v", srv.calls)
	}
}
