package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"

	"github.com/taufinity/cli/internal/api"
)

// ---- helpers shared by the review tests ----

type recordedReq struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// fakeStudio stands in for Studio: it records every request and answers the
// two code review endpoints.
type fakeStudio struct {
	srv *httptest.Server
	mu  sync.Mutex
	req []recordedReq

	reviewStatus int    // 0 = 200 with reviewLines
	reviewBody   string // body for a non-200, or the NDJSON for a 200
	settingsCode int    // 0 = 200
	settingsBody string
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "internal", "review", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newFakeStudio(t *testing.T, ndjson string) *fakeStudio {
	t.Helper()
	f := &fakeStudio{
		reviewBody:   ndjson,
		settingsBody: `{"organization_id":12,"enabled":true,"max_reviews_per_day":50,"daily_pct_used":6,"monthly_pct_used":2}`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.req = append(f.req, recordedReq{r.Method, r.URL.Path, r.Header.Clone(), body})
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-review/reviews":
			if f.reviewStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.reviewStatus)
				_, _ = io.WriteString(w, f.reviewBody)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, line := range strings.SplitAfter(f.reviewBody, "\n") {
				if line == "" {
					continue
				}
				_, _ = io.WriteString(w, line)
				w.(http.Flusher).Flush()
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-review/settings":
			w.Header().Set("Content-Type", "application/json")
			if f.settingsCode != 0 {
				w.WriteHeader(f.settingsCode)
			}
			_, _ = io.WriteString(w, f.settingsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStudio) requests() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedReq(nil), f.req...)
}

func (f *fakeStudio) posts() []recordedReq {
	var out []recordedReq
	for _, r := range f.requests() {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func putFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newReviewTestRepo makes a repo on branch feat: main has main.go, feat has one
// uncommitted change in main.go plus a Bitbucket origin.
func newReviewTestRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := filepath.Join(t.TempDir(), "site")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "init", "-q", "-b", "main")
	testGit(t, dir, "config", "user.name", "t")
	testGit(t, dir, "config", "user.email", "t@example.invalid")
	putFile(t, dir, "main.go", "package main\n")
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "-m", "init")
	testGit(t, dir, "checkout", "-q", "-b", "feat")
	testGit(t, dir, "remote", "add", "origin", "git@bitbucket.org:acme/site.git")
	putFile(t, dir, "main.go", "package main\n\nfunc main() { unchecked() }\n")
	return dir
}

// resetReviewState puts every flag the review tests touch back to its default,
// since cobra keeps flag values between Execute calls.
func resetReviewState(t *testing.T) {
	t.Helper()
	resetGlobals(t)
	flagEnv = ""
	for _, set := range []*pflag.FlagSet{reviewCmd.Flags(), reviewHookCmd.Flags()} {
		set.VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
}

// runReviewIn runs `taufinity <args>` with dir as the working directory and
// returns what it wrote to stdout and stderr and its error.
func runReviewIn(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetReviewState(t)
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	rootCmd.SetArgs(args)
	err = rootCmd.Execute()

	outW.Close()
	errW.Close()
	wg.Wait()
	os.Stdout, os.Stderr = oldOut, oldErr
	return outBuf.String(), errBuf.String(), err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

const fakeAPIKey = "test-api-key-value" // gitleaks:allow -- clearly fake test value

func useAPIKey(t *testing.T) { t.Setenv(api.APIKeyEnv, fakeAPIKey) }

// ---- T9 ----

func TestReviewCommand(t *testing.T) {
	result := readFixture(t, "stream_result.ndjson")

	t.Run("prints_markdown_and_summary", func(t *testing.T) {
		useAPIKey(t)
		dir := newReviewTestRepo(t)
		f := newFakeStudio(t, result)
		out, errOut, err := runReviewIn(t, dir, "--api-url", f.srv.URL, "review")
		if err != nil {
			t.Fatalf("err %v\n%s", err, errOut)
		}
		if !strings.Contains(out, "## Summary\nOne problem.") || strings.Contains(out, "Review:") {
			t.Errorf("stdout should hold the review only:\n%s", out)
		}
		for _, want := range []string{"1 high", "0 critical", "verdict: request_changes", "Reviewing (reviewing)", "still reviewing"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q:\n%s", want, errOut)
			}
		}
		if strings.Contains(errOut, "(partial)") {
			t.Error("a complete review is not partial")
		}
	})

	t.Run("fail_on_critical_exit_0_for_high", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "critical")
		if exitCode(err) != 0 {
			t.Fatalf("exit %d", exitCode(err))
		}
	})

	t.Run("fail_on_high_exit_1", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		out, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "high")
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 1 || ee.Err != nil {
			t.Fatalf("want a silent exit 1, got %v", err)
		}
		if !strings.Contains(out, "## Summary") || strings.Contains(errOut, "Error") {
			t.Errorf("exit 1 must still print the review and no error:\nout=%s\nerr=%s", out, errOut)
		}
	})

	t.Run("fail_on_none_exit_0", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "none")
		if exitCode(err) != 0 {
			t.Fatalf("exit %d", exitCode(err))
		}
	})

	// Production on 2026-11-12: verdict request_changes, every count 0, no
	// findings, a Critical in the markdown. The service lost the counts; the
	// gate must not pass on that.
	zeroCounts := readFixture(t, "stream_result_zero_counts.ndjson")
	for _, failOn := range []string{"critical", "high", "medium"} {
		t.Run("request_changes_without_counts_fails_"+failOn, func(t *testing.T) {
			useAPIKey(t)
			f := newFakeStudio(t, zeroCounts)
			_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", failOn)
			if exitCode(err) != 1 {
				t.Fatalf("exit %d, want 1\n%s", exitCode(err), errOut)
			}
			if !strings.Contains(errOut, "no critical or high finding") {
				t.Errorf("stderr does not say why it failed:\n%s", errOut)
			}
		})
	}

	t.Run("request_changes_without_counts_fail_on_none_exit_0", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, zeroCounts)
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "none")
		if exitCode(err) != 0 {
			t.Fatalf("exit %d", exitCode(err))
		}
	})

	t.Run("fail_on_invalid_exit_2", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "bogus")
		if exitCode(err) != 2 || len(f.requests()) != 0 {
			t.Fatalf("exit %d, requests %d", exitCode(err), len(f.requests()))
		}
	})

	t.Run("error_line_exit_2", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, readFixture(t, "stream_error.ndjson"))
		out, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if exitCode(err) != 2 {
			t.Fatalf("exit %d", exitCode(err))
		}
		if !strings.Contains(errOut, "no_eu_model") || !strings.Contains(errOut, "no EU model backend is configured") || out != "" {
			t.Errorf("out=%q err=%q", out, errOut)
		}
	})

	t.Run("stream_without_terminal_exit_2", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, `{"type":"accepted","request_id":"r"}`+"\n"+`{"stage":"reviewing","type":"progress"}`+"\n")
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "without a result") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("json_prints_terminal_line", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		out, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--json")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
			t.Fatalf("want exactly one line:\n%s", out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil || got["type"] != "result" || got["verdict"] != "request_changes" {
			t.Fatalf("not the terminal line: %v %v", err, got)
		}
	})

	t.Run("no_changes_no_request", func(t *testing.T) {
		useAPIKey(t)
		dir := newReviewTestRepo(t)
		testGit(t, dir, "checkout", "-q", "--", "main.go")
		f := newFakeStudio(t, result)
		_, errOut, err := runReviewIn(t, dir, "--api-url", f.srv.URL, "review")
		if err != nil || len(f.requests()) != 0 || !strings.Contains(errOut, "No changes") {
			t.Fatalf("err=%v requests=%d stderr=%q", err, len(f.requests()), errOut)
		}
	})

	t.Run("no_change_under_paths_no_request", func(t *testing.T) {
		useAPIKey(t)
		dir := newReviewTestRepo(t)
		putFile(t, dir, "docs/a.md", "x\n")
		testGit(t, dir, "add", "docs")
		testGit(t, dir, "commit", "-q", "-m", "docs")
		f := newFakeStudio(t, result)
		_, _, err := runReviewIn(t, dir, "--api-url", f.srv.URL, "review", "nowhere/")
		if err != nil || len(f.requests()) != 0 {
			t.Fatalf("err=%v requests=%d", err, len(f.requests()))
		}
	})

	t.Run("dry_run_sends_nothing", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		f := newFakeStudio(t, result)
		out, _, err := runReviewIn(t, dir, "--api-url", f.srv.URL, "--dry-run", "review")
		if err != nil || len(f.requests()) != 0 {
			t.Fatalf("err=%v requests=%d", err, len(f.requests()))
		}
		for _, want := range []string{"mode: diff", "base: main", "repo: acme/site", "files: 1", "diff bytes:", "skipped files: 0", "masked: 0"} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "unchecked()") {
			t.Error("dry-run printed file content")
		}
	})

	t.Run("whole_without_paths_exit_2", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--whole")
		if exitCode(err) != 2 || len(f.requests()) != 0 {
			t.Fatalf("exit %d requests %d", exitCode(err), len(f.requests()))
		}
	})

	t.Run("not_signed_in_exit_2_hint", func(t *testing.T) {
		t.Setenv(api.APIKeyEnv, "")
		f := newFakeStudio(t, result)
		_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if exitCode(err) != 2 || len(f.requests()) != 0 || !strings.Contains(errOut, "taufinity auth login") {
			t.Fatalf("exit %d requests %d stderr %q", exitCode(err), len(f.requests()), errOut)
		}
	})
}

// ---- T10 ----

func TestReviewRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		message string
		hint    string
	}{
		{"daily_budget_reached", 429, `{"error":"daily_budget_reached","message":"The daily code review budget has been reached. Reviews continue tomorrow, or ask for a higher budget."}`,
			"The daily code review budget has been reached. Reviews continue tomorrow, or ask for a higher budget.",
			"try again tomorrow, or ask your organisation admin for a higher limit"},
		{"monthly_budget_reached", 429, `{"error":"monthly_budget_reached","message":"The monthly code review budget has been reached. Reviews continue next month, or ask for a higher budget."}`,
			"The monthly code review budget has been reached. Reviews continue next month, or ask for a higher budget.",
			"try again next month, or ask your organisation admin"},
		{"daily_review_limit", 429, `{"error":"daily_review_limit","message":"The daily limit of 50 code reviews has been reached. Reviews continue tomorrow."}`,
			"The daily limit of 50 code reviews has been reached. Reviews continue tomorrow.",
			"try again tomorrow, or ask your organisation admin for a higher limit"},
		{"not_enabled", 403, `{"error":"not_enabled","message":"Code review is not switched on for this organisation yet. Ask your Taufinity contact to enable it."}`,
			"Code review is not switched on for this organisation yet. Ask your Taufinity contact to enable it.",
			"ask your Taufinity contact to switch on code review"},
		{"org_pending_gate", 403, `{"error":"org_pending","message":"This organisation is waiting for approval."}`,
			"This organisation is waiting for approval.", ""},
		{"unauthorized_hints_login", 401, `{"error":"unauthorized","message":"Authentication required."}`,
			"Authentication required.", "run `taufinity auth login` (add --env staging for staging)"},
		{"cli_unavailable_503", 503, `{"error":"code_review_cli_unavailable","message":"Code review from the command line is not available right now."}`,
			"Code review from the command line is not available right now.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useAPIKey(t)
			f := newFakeStudio(t, c.body)
			f.reviewStatus = c.status
			out, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
			if exitCode(err) != 2 {
				t.Fatalf("exit %d", exitCode(err))
			}
			if n := len(f.posts()); n != 1 {
				t.Fatalf("Studio saw %d POSTs, want exactly 1 (no retry)", n)
			}
			if !strings.Contains(errOut, c.message) || out != "" {
				t.Errorf("out=%q stderr=%q", out, errOut)
			}
			if c.hint != "" && !strings.Contains(errOut, "Hint: "+c.hint) {
				t.Errorf("stderr lacks the hint %q:\n%s", c.hint, errOut)
			}
			if c.hint == "" && strings.Contains(errOut, "Hint:") {
				t.Errorf("unexpected hint:\n%s", errOut)
			}
		})
	}

	t.Run("stored_login_401_is_one_post", func(t *testing.T) {
		t.Setenv(api.APIKeyEnv, "")
		dir := newReviewTestRepo(t)
		f := newFakeStudio(t, `{"error":"unauthorized","message":"Authentication required."}`)
		f.reviewStatus = 401
		seedRotatedSession(t, f.srv.URL)
		_, errOut, err := runReviewIn(t, dir, "--api-url", f.srv.URL, "review")
		if exitCode(err) != 2 {
			t.Fatalf("exit %d", exitCode(err))
		}
		reviews := 0
		for _, r := range f.posts() {
			if r.Path == "/api/code-review/reviews" {
				reviews++
			}
		}
		if reviews != 1 {
			t.Fatalf("Studio saw %d review POSTs with a stored login, want exactly 1", reviews)
		}
		if !strings.Contains(errOut, "Hint: run `taufinity auth login` (add --env staging for staging)") {
			t.Errorf("stderr:\n%s", errOut)
		}
	})

	t.Run("non_json_body_shows_status", func(t *testing.T) {
		useAPIKey(t)
		page := "<html><body>Bad Gateway " + strings.Repeat("x", 400) + "TAIL-MARKER</body></html>"
		f := newFakeStudio(t, page)
		f.reviewStatus = 502
		_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if exitCode(err) != 2 || len(f.posts()) != 1 {
			t.Fatalf("exit %d posts %d", exitCode(err), len(f.posts()))
		}
		if !strings.Contains(errOut, "HTTP 502") || !strings.Contains(errOut, "<html><body>Bad Gateway") || strings.Contains(errOut, "TAIL-MARKER") {
			t.Errorf("want the status and the first 200 bytes only:\n%s", errOut)
		}
	})
}

// ---- T12 ----

func TestReviewRequestBody(t *testing.T) {
	result := readFixture(t, "stream_result.ndjson")
	wantKeys := []string{"base_ref", "diff", "files", "head_sha", "mode", "repo", "title"}

	capture := func(t *testing.T, dir string, args ...string) (raw []byte, body map[string]any, stderr string) {
		t.Helper()
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, errOut, err := runReviewIn(t, dir, append([]string{"--api-url", f.srv.URL, "review"}, args...)...)
		if err != nil {
			t.Fatalf("err %v\n%s", err, errOut)
		}
		posts := f.posts()
		if len(posts) != 1 {
			t.Fatalf("posts %d", len(posts))
		}
		if ct := posts[0].Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type %q", ct)
		}
		if err := json.Unmarshal(posts[0].Body, &body); err != nil {
			t.Fatal(err)
		}
		return posts[0].Body, body, errOut
	}
	assertKeys := func(t *testing.T, body map[string]any) {
		t.Helper()
		var got []string
		for k := range body {
			got = append(got, k)
		}
		if len(got) != len(wantKeys) {
			t.Errorf("body keys %v, want %v", got, wantKeys)
		}
		for _, k := range wantKeys {
			if _, ok := body[k]; !ok {
				t.Errorf("body lacks %q", k)
			}
		}
	}

	t.Run("diff_mode_body", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		_, body, _ := capture(t, dir)
		assertKeys(t, body)
		if body["mode"] != "diff" || body["repo"] != "acme/site" || body["base_ref"] != "main" || body["title"] != "feat" {
			t.Errorf("body %v", body)
		}
		if sha, _ := body["head_sha"].(string); len(sha) != 40 {
			t.Errorf("head_sha %q", sha)
		}
		if d, _ := body["diff"].(string); !strings.Contains(d, "+func main() { unchecked() }") {
			t.Errorf("diff %q", d)
		}
		files := body["files"].(map[string]any)
		if files["main.go"] != "package main\n\nfunc main() { unchecked() }\n" || len(files) != 1 {
			t.Errorf("files %v", files)
		}
	})

	t.Run("repo_flag_overrides", func(t *testing.T) {
		_, body, _ := capture(t, newReviewTestRepo(t), "--repo", "custom/name")
		if body["repo"] != "custom/name" {
			t.Errorf("repo %v", body["repo"])
		}
	})

	t.Run("whole_mode_body", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		putFile(t, dir, "lib/util.go", "package lib\n")
		_, body, _ := capture(t, dir, "--whole", "lib")
		assertKeys(t, body)
		if body["mode"] != "whole" {
			t.Errorf("mode %v", body["mode"])
		}
		if d, _ := body["diff"].(string); !strings.Contains(d, "--- /dev/null\n+++ b/lib/util.go") {
			t.Errorf("diff %q", d)
		}
		if files := body["files"].(map[string]any); files["lib/util.go"] != "package lib\n" || len(files) != 1 {
			t.Errorf("files %v", files)
		}
	})

	t.Run("paths_reach_the_body_only_as_files", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		putFile(t, dir, "src/a.go", "package a\n")
		putFile(t, dir, "docs/b.md", "b\n")
		_, body, _ := capture(t, dir, "src")
		assertKeys(t, body)
		files := body["files"].(map[string]any)
		if _, ok := files["src/a.go"]; !ok || len(files) != 1 {
			t.Errorf("files %v", files)
		}
		if strings.Contains(body["diff"].(string), "docs/b.md") || strings.Contains(body["diff"].(string), "main.go") {
			t.Errorf("diff not narrowed: %q", body["diff"])
		}
	})

	t.Run("no_secret_in_body", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		putFile(t, dir, "main.go", "package main\n\nconst t = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\"\n") // gitleaks:allow -- clearly fake test value
		raw, _, errOut := capture(t, dir)
		if bytes.Contains(raw, []byte("ghp_abcdefghijklmnopqrstuvwxyz0123456789")) { // gitleaks:allow -- clearly fake test value
			t.Fatal("the planted token reached the request")
		}
		if !bytes.Contains(raw, []byte("[MASKED]")) {
			t.Error("the body should carry the mask")
		}
		if !strings.Contains(errOut, "masked 2 secret value(s)") || strings.Contains(errOut, "ghp_") {
			t.Errorf("stderr should count the masked values, never print them:\n%s", errOut)
		}
	})

	t.Run("excluded_env_not_in_body", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		putFile(t, dir, ".env", "DATABASE_URL=postgres://u:pw-unique-marker@h/db\n") // gitleaks:allow -- clearly fake test value
		raw, _, errOut := capture(t, dir)
		if bytes.Contains(raw, []byte("pw-unique-marker")) || bytes.Contains(raw, []byte(".env")) {
			t.Fatal(".env reached the request")
		}
		if !strings.Contains(errOut, "skipped .env (excluded)") {
			t.Errorf("stderr should list the skipped file:\n%s", errOut)
		}
	})
}

// ---- T13 ----

func TestReviewTalksOnlyToStudio(t *testing.T) {
	result := readFixture(t, "stream_result.ndjson")

	t.Run("only_studio_paths", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		if _, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review"); err != nil {
			t.Fatal(err)
		}
		reqs := f.requests()
		if len(reqs) != 2 || reqs[0].Method != "POST" || reqs[0].Path != "/api/code-review/reviews" ||
			reqs[1].Method != "GET" || reqs[1].Path != "/api/code-review/settings" {
			t.Fatalf("requests: %+v", reqs)
		}
	})

	t.Run("env_staging_targets_staging", func(t *testing.T) {
		dir := newReviewTestRepo(t)
		out, _, err := runReviewIn(t, dir, "--env", "staging", "--dry-run", "review")
		if err != nil {
			t.Fatal(err)
		}
		if got := newReviewClient().BaseURL(); got != StagingAPIURL {
			t.Fatalf("review client targets %q, want %q", got, StagingAPIURL)
		}
		if !strings.Contains(out, StagingAPIURL+"/api/code-review/reviews") {
			t.Errorf("dry-run should name the target:\n%s", out)
		}
	})

	t.Run("org_flag_sets_header", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		if _, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "--org", "12", "review"); err != nil {
			t.Fatal(err)
		}
		reqs := f.requests()
		if len(reqs) != 2 {
			t.Fatalf("requests %d", len(reqs))
		}
		for _, r := range reqs {
			if r.Header.Get("X-Organization-ID") != "12" {
				t.Errorf("%s %s sent org %q", r.Method, r.Path, r.Header.Get("X-Organization-ID"))
			}
		}
		if reqs[0].Header.Get("X-API-Key") != fakeAPIKey || reqs[0].Header.Get("Authorization") != "" {
			t.Errorf("auth headers %v", reqs[0].Header)
		}
	})

	t.Run("no_endpoint_flag", func(t *testing.T) {
		reviewCmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			for _, bad := range []string{"url", "endpoint", "host"} {
				if strings.Contains(strings.ToLower(f.Name), bad) {
					t.Errorf("flag --%s could point the review elsewhere", f.Name)
				}
			}
		})
	})

	t.Run("no_direct_call_to_the_review_service", func(t *testing.T) {
		needle := "/review/" + "v1"
		files, _ := filepath.Glob("review*.go")
		more, _ := filepath.Glob("mcp_stdio_review*.go")
		files = append(files, more...)
		files = append(files, "../internal/api/stream.go")
		rev, _ := filepath.Glob("../internal/review/*.go")
		files = append(files, rev...)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), needle) {
				t.Errorf("%s mentions the review service path", f)
			}
		}
	})
}

func TestReviewRefusesCleartext(t *testing.T) {
	useAPIKey(t)
	for _, url := range []string{"http://studio.example.invalid", "ftp://localhost"} {
		_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", url, "review")
		if exitCode(err) != 2 || !strings.Contains(errOut+err.Error(), "https") {
			t.Errorf("%s: exit %d err %v", url, exitCode(err), err)
		}
	}
	if err := requireSecureStudioURL("http://127.0.0.1:8080"); err != nil {
		t.Errorf("loopback http should be allowed: %v", err)
	}
	if err := requireSecureStudioURL(StagingAPIURL); err != nil {
		t.Errorf("https should be allowed: %v", err)
	}
}
