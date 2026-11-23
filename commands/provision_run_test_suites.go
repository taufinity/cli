// provision_run_test_suites.go — `provision run-test-suites`.
//
// Triggers every test suite in the org, polls each run to completion, prints a
// summary, and exits non-zero if any suite failed. Intended as a CI gate after
// `provision apply`: the config landed, now prove the suites still pass.
//
// Suites run one at a time rather than in parallel. The API caps concurrent runs
// per org, so firing them all at once starts failing as soon as an org has more
// suites than the cap.
package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	runSuitesOrgSlug  string
	runSuitesAPIKey   string
	runSuitesTimeout  int
	runSuitesPriority string
	runSuitesInterval = 5 * time.Second // var so tests can shorten it

	// A poll can land while the service rolls over to a new revision: the old
	// instance resets the connection or the proxy answers 502/503. That says
	// nothing about the suite, so the poll is retried with backoff for up to
	// runSuitesTransportBudget of continuous failure. Vars so tests can shorten them.
	runSuitesTransportBudget = 5 * time.Minute
	// Overall cap on time spent waiting out outages for one run, across all
	// outages, so a flapping API (one good poll, one bad poll, repeat) cannot
	// extend the wait forever. Worst case per run: --timeout + this.
	runSuitesTransportTotalBudget = 8 * time.Minute
	runSuitesTransportBackoff     = 2 * time.Second
	runSuitesTransportMaxWait     = 30 * time.Second
)

// transportError marks a poll that never got an answer about the run: the
// connection failed or the service was briefly unavailable. It is deliberately
// a different failure from a suite that did not pass.
type transportError struct {
	runUUID string
	budget  time.Duration
	last    string
}

func (e *transportError) Error() string {
	return fmt.Sprintf("TRANSPORT FAILURE, not a suite result: could not reach the API to read run %s for %s (last error: %s); the suite outcome is unknown",
		e.runUUID, e.budget, e.last)
}

// isTransientPoll reports whether a poll response says nothing about the run:
// the request never completed (err set, or status 0), or a gateway answered
// 408/429/502/503/504 while an instance was being replaced.
func isTransientPoll(status int, err error) bool {
	if err != nil || status == 0 {
		return true
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// testSuiteRun is the subset of the trigger and poll responses we need.
type testSuiteRun struct {
	UUID        string `json:"uuid"`
	Status      string `json:"status"`
	PassedCount int    `json:"passed_count"`
	FailedCount int    `json:"failed_count"`
	ErrorCount  int    `json:"error_count"`
	TotalCount  int    `json:"total_count"`
	Error       string `json:"error,omitempty"`
}

var provisionRunTestSuitesCmd = &cobra.Command{
	Use:   "run-test-suites",
	Short: "Run every test suite in the org and report pass/fail",
	Long: `Trigger each test suite in the target org, wait for it to finish, and
print a pass/fail summary. Exits non-zero if any suite has a failing case, so it
can be used as a CI gate after 'provision apply'.

Suites are run sequentially to stay under the per-org concurrent-run limit.

Use --priority to run only part of the set (for example only the must-pass
suites on every push, and the full set nightly).`,
	RunE: runProvisionRunTestSuites,
}

func init() {
	provisionCmd.AddCommand(provisionRunTestSuitesCmd)
	provisionRunTestSuitesCmd.Flags().StringVar(&runSuitesOrgSlug, "org", "", "Organization slug (required)")
	provisionRunTestSuitesCmd.Flags().StringVar(&runSuitesAPIKey, "api-key", "", "Bootstrap admin API key (overrides TAUFINITY_ADMIN_TOKEN env)")
	provisionRunTestSuitesCmd.Flags().IntVar(&runSuitesTimeout, "timeout", 300, "Seconds to wait for each suite run to complete")
	provisionRunTestSuitesCmd.Flags().StringVar(&runSuitesPriority, "priority", "all", "Filter suites by priority: must|recommend|optional|all")
	_ = provisionRunTestSuitesCmd.MarkFlagRequired("org")
}

func runProvisionRunTestSuites(cmd *cobra.Command, args []string) error {
	key := runSuitesAPIKey
	if key == "" {
		var err error
		if key, err = resolveProvisionAPIKey(); err != nil {
			return err
		}
	}
	c := newProvisionClient(GetAPIURL(), key, false)
	orgID, err := resolveProvisionOrgID(c, runSuitesOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", runSuitesOrgSlug, err)
	}
	return runTestSuites(c, orgID, runSuitesOrgSlug, runSuitesPriority, time.Duration(runSuitesTimeout)*time.Second)
}

// runTestSuites is the testable core: list → filter → trigger → poll → summarize.
func runTestSuites(c *provisionClient, orgID uint, orgSlug, priority string, timeout time.Duration) error {
	body, status, err := c.getForOrg("/test-suites/", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list test suites", status, body, err)
	}
	var all []testSuiteListItem
	if err := unmarshalListEnvelope(body, &all); err != nil {
		return fmt.Errorf("parse test suites: %w", err)
	}

	suites := all
	if priority != "all" {
		suites = nil
		for _, s := range all {
			if slugPriorityOf(s.Slug) == priority {
				suites = append(suites, s)
			}
		}
	}

	if len(suites) == 0 {
		if priority != "all" {
			fmt.Printf("no %q test suites found for org %s\n", priority, orgSlug)
		} else {
			fmt.Printf("no test suites found for org %s\n", orgSlug)
		}
		return nil
	}

	label := ""
	if priority != "all" {
		label = fmt.Sprintf(" (priority=%s)", priority)
	}
	fmt.Printf("==> running %d test suite(s) for %s%s (sequential)\n", len(suites), orgSlug, label)

	type suiteResult struct {
		name string
		run  testSuiteRun
		err  error
	}
	results := make([]suiteResult, len(suites))

	for i, suite := range suites {
		results[i].name = suite.Name

		rb, st, err := c.writeForOrg("POST", "/test-suites/"+suite.UUID+"/run", nil, orgID)
		if err != nil || st >= 300 {
			results[i].err = provisionAPIErr("trigger failed", st, rb, err)
			continue
		}
		var triggered testSuiteRun
		if err := json.Unmarshal(rb, &triggered); err != nil || triggered.UUID == "" {
			results[i].err = fmt.Errorf("unexpected trigger response: %s", provisionSummarize(rb))
			continue
		}
		fmt.Printf("  [%d/%d] %-45s → %s … ", i+1, len(suites), suite.Name, triggered.UUID)
		results[i].run = triggered

		if err := pollUntilDone(c, orgID, triggered.UUID, timeout, &results[i].run); err != nil {
			results[i].err = err
			fmt.Println("ERROR")
		} else if !results[i].run.allPassed() {
			fmt.Printf("FAIL (%d/%d passed)\n", results[i].run.PassedCount, results[i].run.TotalCount)
		} else {
			fmt.Printf("PASS (%d/%d)\n", results[i].run.PassedCount, results[i].run.TotalCount)
		}
	}

	fmt.Println()
	fmt.Println("=== test suite results ===")
	anyFailed := false
	for _, r := range results {
		switch {
		case r.err != nil:
			fmt.Printf("  FAIL  %s — %v\n", r.name, r.err)
			anyFailed = true
		case r.run.Status != "completed":
			fmt.Printf("  FAIL  %s — run ended with status %q\n", r.name, r.run.Status)
			anyFailed = true
		case !r.run.allPassed():
			fmt.Printf("  FAIL  %s — %d/%d cases passed (%d failed, %d errored, %d without a result)\n",
				r.name, r.run.PassedCount, r.run.TotalCount, r.run.FailedCount, r.run.ErrorCount,
				r.run.TotalCount-r.run.PassedCount-r.run.FailedCount-r.run.ErrorCount)
			anyFailed = true
		default:
			fmt.Printf("  PASS  %s — %d/%d cases passed\n", r.name, r.run.PassedCount, r.run.TotalCount)
		}
	}

	if anyFailed {
		return fmt.Errorf("one or more test suites failed")
	}
	return nil
}

// allPassed reports whether every case of a completed run passed. A case the
// server could not execute counts in error_count rather than failed_count, and
// a run can also carry fewer verdicts than cases; neither is a pass. Checking
// failed_count alone let a must suite with ten errored cases out of fifteen
// read as PASS.
func (r testSuiteRun) allPassed() bool {
	return r.TotalCount > 0 && r.PassedCount == r.TotalCount && r.FailedCount == 0 && r.ErrorCount == 0
}

// pollUntilDone polls the run until it reaches a terminal status or the timeout
// expires. Transport failures and gateway errors are retried with backoff for up
// to runSuitesTransportBudget of continuous failure (and runSuitesTransportTotalBudget
// across all outages of the run), and the time spent waiting
// out an outage does not count against the suite timeout. A poll that never
// recovers returns a *transportError; any other non-200 is returned at once.
func pollUntilDone(c *provisionClient, orgID uint, runUUID string, timeout time.Duration, out *testSuiteRun) error {
	deadline := time.Now().Add(timeout)
	var outageStart time.Time
	var waitedBefore time.Duration // outage time from earlier, recovered outages
	wait := runSuitesTransportBackoff
	for {
		body, status, err := c.getForOrg("/test-runs/"+runUUID, orgID)
		if isTransientPoll(status, err) {
			now := time.Now()
			if outageStart.IsZero() {
				outageStart = now
			}
			last := provisionAPIErr("poll failed", status, body, err).Error()
			if now.Sub(outageStart)+wait > runSuitesTransportBudget ||
				waitedBefore+now.Sub(outageStart)+wait > runSuitesTransportTotalBudget {
				return &transportError{runUUID: runUUID, budget: runSuitesTransportBudget, last: last}
			}
			fmt.Printf("\n    poll interrupted (%s); retrying in %s ", last, wait)
			time.Sleep(wait)
			if wait *= 2; wait > runSuitesTransportMaxWait {
				wait = runSuitesTransportMaxWait
			}
			continue
		}
		if !outageStart.IsZero() {
			outage := time.Since(outageStart)
			deadline = deadline.Add(outage)
			waitedBefore += outage
			outageStart = time.Time{}
			wait = runSuitesTransportBackoff
		}
		if err != nil || status != 200 {
			return provisionAPIErr("poll failed", status, body, err)
		}
		var resp struct {
			Run testSuiteRun `json:"run"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse poll response: %w", err)
		}
		*out = resp.Run
		if resp.Run.Status == "completed" || resp.Run.Status == "failed" {
			return nil
		}
		if !time.Now().Add(runSuitesInterval).Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for run %s", timeout, runUUID)
		}
		time.Sleep(runSuitesInterval)
	}
}

// slugPriorityOf reads the priority level out of a suite slug. Two forms are
// accepted, because the YAML author writes one and the server may normalize it
// into the other (possibly with a collision suffix appended):
//
//	prefix:  must__formula-sanity
//	segment: formula-sanity-must, formula-sanity-must-a1b2c3d4
//
// Slugs that encode no priority default to "recommend".
func slugPriorityOf(slug string) string {
	for _, p := range []string{"must", "recommend", "optional"} {
		if strings.HasPrefix(slug, p+"__") {
			return p
		}
	}
	for _, seg := range strings.Split(slug, "-") {
		switch seg {
		case "must", "recommend", "optional":
			return seg
		}
	}
	return "recommend"
}
