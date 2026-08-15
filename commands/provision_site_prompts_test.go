package commands

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePrompt(t *testing.T, dir, name, body string) {
	t.Helper()
	pd := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// Site-scoped prompts are stored under "<name>__<site_id>", the convention the
// server already resolves ahead of the org-level row. Two sites in one org need
// genuinely different instructions, and an org-level override would hit both.
func TestApplySitePrompts_SuffixesWithSiteID(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	siteDir := t.TempDir()
	writePrompt(t, siteDir, "humanize_refinement.txt", "site specific body")

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applySitePrompts(c, siteDir, 1, "quizreps_com"); err != nil {
		t.Fatalf("applySitePrompts: %v", err)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("want 1 push, got %d: %+v", len(srv.calls), srv.calls)
	}
	if got := srv.calls[0].Name; got != "humanize_refinement__quizreps_com" {
		t.Errorf("name = %q, want humanize_refinement__quizreps_com", got)
	}
	if got := srv.calls[0].Body; got != "site specific body" {
		t.Errorf("body = %q", got)
	}
}

// The org-level path must keep pushing unsuffixed names, or every existing
// override silently becomes a second, differently-named row.
func TestApplyPrompts_OrgLevelNameIsUnsuffixed(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "humanize_refinement.txt", "org body")

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 1); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if len(srv.calls) != 1 || srv.calls[0].Name != "humanize_refinement" {
		t.Errorf("org-level name should not be suffixed, got %+v", srv.calls)
	}
}

// An unchanged body must not be rewritten. Re-applying a spec is routine, and
// a no-op write still burns a version row and an audit entry.
func TestApplyPrompts_UnchangedBodyIsNotPushed(t *testing.T) {
	srv := &promptsTestServer{existing: map[string]string{"already": "same body"}}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "already.txt", "same body")

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 1); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("identical body should not be written, got %+v", srv.calls)
	}
}

// A changed body carries If-Match from the read, so a concurrent edit in the
// read-modify-write window fails loudly instead of being clobbered.
func TestApplyPrompts_ChangedBodySendsIfMatch(t *testing.T) {
	srv := &promptsTestServer{existing: map[string]string{"drifted": "old body"}}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "drifted.txt", "new body")

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 1); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if len(srv.calls) != 1 {
		t.Fatalf("want 1 push, got %d", len(srv.calls))
	}
	if got := srv.ifMatch["drifted"]; got != etagFor("old body") {
		t.Errorf("If-Match = %q, want the etag of the body we read (%q)", got, etagFor("old body"))
	}
}

// Creating a prompt that does not exist yet must not send If-Match — there is
// nothing to match, and the server rejects a conditional create.
func TestApplyPrompts_CreateSendsNoIfMatch(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "brand_new.txt", "body")

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPrompts(c, dir, 1); err != nil {
		t.Fatalf("applyPrompts: %v", err)
	}
	if got := srv.ifMatch["brand_new"]; got != "" {
		t.Errorf("create should be unconditional, got If-Match %q", got)
	}
}

// A 412 must stop the run with an actionable message, not be swallowed as a
// generic upsert failure.
func TestApplyPrompts_ConflictIsReportedClearly(t *testing.T) {
	srv := &promptsTestServer{
		existing:   map[string]string{"contended": "old"},
		conflictOn: "contended",
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "contended.txt", "mine")

	c := newProvisionClient(ts.URL, "test-key", false)
	err := applyPrompts(c, dir, 1)
	if err == nil {
		t.Fatal("expected an error on 412")
	}
	if !strings.Contains(err.Error(), "changed since it was read") {
		t.Errorf("error should explain the conflict, got: %v", err)
	}
}

func TestSitePromptSuffix(t *testing.T) {
	if got := sitePromptSuffix("humanize_refinement", "quizreps_com"); got != "humanize_refinement__quizreps_com" {
		t.Errorf("got %q", got)
	}
}

// A dry-run must not report "pushed=0" under a list of CREATE lines — that
// reads as "nothing will happen", the opposite of what the diff just said.
func TestApplyPrompts_DryRunCountsWouldPush(t *testing.T) {
	srv := &promptsTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	writePrompt(t, dir, "counted.txt", "body")

	out := captureStdout(t, func() {
		c := newProvisionClient(ts.URL, "test-key", true) // dryRun
		if err := applyPrompts(c, dir, 1); err != nil {
			t.Fatalf("applyPrompts: %v", err)
		}
	})

	if !strings.Contains(out, "would push=1") {
		t.Errorf("dry-run summary should report 1 would-push, got:\n%s", out)
	}
	if len(srv.calls) != 0 {
		t.Errorf("dry-run must not write, got %+v", srv.calls)
	}
}
