package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/review"
)

// ExitError ends the process with a specific exit code. Code 1 with no Err
// means "the review found something at or above --fail-on": the summary has
// been printed already, so main prints nothing. Printed marks an error whose
// text has already been written to stderr.
type ExitError struct {
	Code    int
	Err     error
	Printed bool
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

const (
	// reviewPath is the only endpoint review content is ever sent to, on the
	// Studio API URL that --env, --api-url and the config select.
	reviewPath = "/api/code-review/reviews"
	// reviewMaxBody is Studio's request limit (8 MiB).
	reviewMaxBody = 8 << 20
)

var (
	reviewWhole  bool
	reviewBase   string
	reviewStaged bool
	reviewFailOn string
	reviewJSON   bool
	reviewRepo   string
)

var reviewCmd = &cobra.Command{
	Use:   "review [paths...]",
	Short: "Review local changes before you push",
	Long: `Review local changes with Taufinity Studio before you push to a branch that deploys.

The CLI reads your local git repository only (GitHub, Bitbucket, GitLab or no
host at all), drops files that must never leave your machine, masks secrets,
and sends the rest to Taufinity Studio. The result streams back: the review on
stdout, progress and a one-line summary on stderr.

Default: the working tree (committed and uncommitted changes, plus new files
that are not ignored) compared with the merge-base of the current branch's
upstream, else origin/main, else main. --staged compares the index instead,
--base picks another ref. Paths limit the review to those files or folders.

--whole reviews the current content of the files under the given paths,
changed or not.

Never sent: .env files, keys and certificates, databases, node_modules, vendor,
dist, build, minified files, source maps, lock files, binaries and files over
1 MiB. Secrets found in what is sent are replaced by [MASKED].

Exit codes: 0 no finding at or above --fail-on, 1 a finding at or above it,
2 an error or refusal. In CI set TAUFINITY_API_KEY.

Examples:
  taufinity review
  taufinity review src/ docs/
  taufinity review --staged --fail-on high
  taufinity review --whole src/api
  taufinity review --env staging`,
	Args: cobra.ArbitraryArgs,
	RunE: runReview,
}

func init() {
	rootCmd.AddCommand(reviewCmd)
	f := reviewCmd.Flags()
	f.BoolVar(&reviewWhole, "whole", false, "Review the current content of the files under the given paths, changed or not")
	f.StringVar(&reviewBase, "base", "", "Compare against this ref instead of the upstream, origin/main or main")
	f.BoolVar(&reviewStaged, "staged", false, "Review the staged changes instead of the working tree")
	f.StringVar(&reviewFailOn, "fail-on", "critical", "Exit 1 when a finding has at least this severity: critical, high, medium or none")
	f.BoolVar(&reviewJSON, "json", false, "Print only the final result line as JSON on stdout")
	f.StringVar(&reviewRepo, "repo", "", "Repository name to report (default: owner/name of origin, else the folder name)")
}

// newReviewClient builds the client like newKnowledgeClient: --env, --api-url
// and --org all work through it.
func newReviewClient() *api.Client {
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	client.SetDryRun(IsDryRun())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}
	return client
}

// reviewParams is one review request: the flags of the command, or the
// arguments of the MCP tool.
type reviewParams struct {
	Paths  []string
	Whole  bool
	Staged bool
	Base   string
	Repo   string
}

// reviewOutcome is what a review produced.
type reviewOutcome struct {
	Payload   review.Payload
	NoChanges bool
	DryRun    bool
	Terminal  review.Terminal
	Budget    string
}

// studioRefusal is a non-200 answer from Studio before any streaming.
type studioRefusal struct {
	Status        int
	Code, Message string
}

func (e *studioRefusal) Error() string { return e.Message }

// Hint is the one-line next step for a refusal code, "" when there is none.
func (e *studioRefusal) Hint() string {
	switch e.Code {
	case "daily_budget_reached", "daily_review_limit":
		return "try again tomorrow, or ask your organisation admin for a higher limit"
	case "monthly_budget_reached":
		return "try again next month, or ask your organisation admin"
	case "not_enabled":
		return "ask your Taufinity contact to switch on code review"
	case "unauthorized":
		return "run `taufinity auth login` (add --env staging for staging)"
	}
	return ""
}

// runReview collects, sends and prints. Every failure is exit code 2.
func runReview(cmd *cobra.Command, args []string) error {
	switch reviewFailOn {
	case "critical", "high", "medium", "none":
	default:
		return &ExitError{Code: 2, Err: fmt.Errorf("--fail-on must be critical, high, medium or none, not %q", reviewFailOn)}
	}
	dir, err := os.Getwd()
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()

	run := reviewRun{
		params: reviewParams{Paths: args, Whole: reviewWhole, Staged: reviewStaged, Base: reviewBase, Repo: reviewRepo},
		dir:    dir,
		client: newReviewClient(),
		log:    os.Stderr,
		quiet:  IsQuiet(),
		dryRun: IsDryRun(),
	}
	out, err := run.execute(ctx)
	if err != nil {
		var refusal *studioRefusal
		if errors.As(err, &refusal) {
			fmt.Fprintln(os.Stderr, refusal.Message)
			if h := refusal.Hint(); h != "" {
				fmt.Fprintln(os.Stderr, "Hint: "+h)
			}
			return &ExitError{Code: 2, Err: err, Printed: true}
		}
		return &ExitError{Code: 2, Err: err}
	}
	if out.NoChanges {
		fmt.Fprintln(os.Stderr, "No changes to review.")
		return nil
	}
	if out.DryRun {
		fmt.Fprint(os.Stdout, dryRunText(out.Payload, GetAPIURL()))
		return nil
	}

	t := out.Terminal
	if reviewJSON {
		os.Stdout.Write(append(append([]byte{}, t.Raw...), '\n'))
	}
	if t.Type == "error" {
		msg := fmt.Sprintf("The review failed (%s): %s", t.Code, t.Message)
		fmt.Fprintln(os.Stderr, msg)
		return &ExitError{Code: 2, Err: errors.New(msg), Printed: true}
	}
	if !reviewJSON {
		fmt.Fprint(os.Stdout, ensureNewline(t.Markdown))
	}
	fmt.Fprintln(os.Stderr, reviewSummary(t))
	if out.Budget != "" && !IsQuiet() {
		fmt.Fprintln(os.Stderr, out.Budget)
	}
	if exceedsFailOn(t, reviewFailOn) {
		return &ExitError{Code: 1}
	}
	return nil
}

// reviewRun carries what one review needs, so the command and the MCP tool
// share the path from collection to result.
type reviewRun struct {
	params reviewParams
	dir    string
	client *api.Client
	log    io.Writer // progress, skipped files, masked count
	quiet  bool
	dryRun bool
}

func (r reviewRun) logf(format string, args ...any) {
	if r.log != nil {
		fmt.Fprintf(r.log, format+"\n", args...)
	}
}

func (r reviewRun) execute(ctx context.Context) (reviewOutcome, error) {
	p := r.params
	payload, err := review.Collect(ctx, r.dir, review.Options{Base: p.Base, Staged: p.Staged, Whole: p.Whole, Paths: p.Paths})
	if err != nil {
		return reviewOutcome{}, err
	}
	if p.Repo != "" {
		payload.Repo = p.Repo
	}
	for _, s := range payload.Skipped {
		r.logf("skipped %s (%s)", s.Path, s.Reason)
	}
	if payload.Masked > 0 {
		r.logf("masked %d secret value(s) before sending", payload.Masked)
	}
	out := reviewOutcome{Payload: payload}
	if strings.TrimSpace(payload.Diff) == "" {
		out.NoChanges = true
		return out, nil
	}

	body, err := encodeReviewBody(payload)
	if err != nil {
		return out, err
	}
	if len(body) > reviewMaxBody {
		return out, fmt.Errorf("the request is %d bytes, over Studio's limit of %d bytes (8 MiB); narrow the review with paths", len(body), reviewMaxBody)
	}

	if r.dryRun {
		out.DryRun = true
		return out, nil
	}

	if err := requireSecureStudioURL(r.client.BaseURL()); err != nil {
		return out, err
	}
	if os.Getenv(api.APIKeyEnv) == "" && !auth.HasCredentials() {
		return out, &studioRefusal{Status: 401, Code: "unauthorized", Message: "You are not signed in."}
	}

	resp, err := r.client.PostStreamOnce(ctx, reviewPath, body)
	if err != nil {
		if api.SessionEnded(err) {
			return out, &studioRefusal{Status: 401, Code: "unauthorized", Message: err.Error()}
		}
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, refusalFrom(resp.StatusCode, resp.Body)
	}

	t, err := review.ReadStream(resp.Body, r.onEvent)
	if err != nil {
		return out, err
	}
	out.Terminal = t
	if t.Type == "result" {
		out.Budget = reviewBudgetLine(ctx, r.client)
	}
	return out, nil
}

func (r reviewRun) onEvent(e review.Event) {
	if r.quiet {
		return
	}
	switch e.Type {
	case "accepted":
		r.logf("Review accepted (request %s)", e.RequestID)
	case "progress":
		r.logf("Reviewing (%s)", e.Stage)
	case "keepalive":
		r.logf("still reviewing")
	}
}

// requireSecureStudioURL refuses to send code over plain http, except to the
// local machine (a dev server). The API URL can be set by flag, environment or
// config, and a typo there must not put source code on the wire in clear text.
func requireSecureStudioURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the Studio URL %q is not a valid URL", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("refusing to send code to %s: the Studio URL must use https", u.Host)
}

// reviewRequestBody is the wire contract of POST /api/code-review/reviews.
type reviewRequestBody struct {
	Repo    string            `json:"repo"`
	BaseRef string            `json:"base_ref"`
	HeadSHA string            `json:"head_sha"`
	Mode    string            `json:"mode"`
	Title   string            `json:"title"`
	Diff    string            `json:"diff"`
	Files   map[string]string `json:"files"`
}

func encodeReviewBody(p review.Payload) ([]byte, error) {
	files := p.Files
	if files == nil {
		files = map[string]string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep < > & as they are: smaller body, same JSON
	err := enc.Encode(reviewRequestBody{
		Repo: p.Repo, BaseRef: p.BaseRef, HeadSHA: p.HeadSHA, Mode: p.Mode,
		Title: p.Title, Diff: p.Diff, Files: files,
	})
	return bytes.TrimRight(buf.Bytes(), "\n"), err
}

// refusalFrom turns a non-200 answer into a studioRefusal. A body that is not
// the expected JSON gives the HTTP status and at most the first 200 bytes.
func refusalFrom(status int, body io.Reader) error {
	raw, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || (e.Error == "" && e.Message == "") {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		ref := &studioRefusal{Status: status, Message: fmt.Sprintf("Studio answered HTTP %d: %s", status, strings.Join(strings.Fields(string(snippet)), " "))}
		if status == 401 {
			ref.Code = "unauthorized"
		}
		return ref
	}
	if e.Message == "" {
		e.Message = e.Error
	}
	return &studioRefusal{Status: status, Code: e.Error, Message: e.Message}
}

var severityRank = map[string]int{"info": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}

// exceedsFailOn reports whether the result has a finding at or above the
// threshold. Both the findings list and the counts are consulted.
func exceedsFailOn(t review.Terminal, failOn string) bool {
	threshold, ok := map[string]int{"critical": 4, "high": 3, "medium": 2}[failOn]
	if !ok {
		return false // none
	}
	for _, f := range t.Findings {
		if severityRank[strings.ToLower(f.Severity)] >= threshold && severityRank[strings.ToLower(f.Severity)] > 0 {
			return true
		}
	}
	for sev, n := range t.Counts {
		if n > 0 && severityRank[strings.ToLower(sev)] >= threshold && severityRank[strings.ToLower(sev)] > 0 {
			return true
		}
	}
	return false
}

// reviewSummary is the one-line summary: counts per severity, verdict, and
// "partial" when the review stopped early.
func reviewSummary(t review.Terminal) string {
	order := []string{"critical", "high", "medium", "low"}
	parts := make([]string, 0, len(order))
	for _, s := range order {
		parts = append(parts, fmt.Sprintf("%d %s", t.Counts[s], s))
	}
	// A severity the service adds later still shows up.
	var extra []string
	for s := range t.Counts {
		if _, known := severityRank[s]; !known {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	for _, s := range extra {
		parts = append(parts, fmt.Sprintf("%d %s", t.Counts[s], s))
	}
	line := "Review: " + strings.Join(parts, ", ")
	if t.Verdict != "" {
		line += "; verdict: " + t.Verdict
	}
	if t.Partial {
		line += " (partial)"
	}
	return line
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// dryRunText says what would be sent, never the diff or file contents.
func dryRunText(p review.Payload, apiURL string) string {
	return fmt.Sprintf("[dry-run] would POST %s%s\n  mode: %s\n  base: %s\n  repo: %s\n  files: %d\n  diff bytes: %d\n  skipped files: %d\n  masked: %d\n",
		apiURL, reviewPath, p.Mode, p.BaseRef, p.Repo, len(p.Files), len(p.Diff), len(p.Skipped), p.Masked)
}
