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
)

// testSuiteRun is the subset of the trigger and poll responses we need.
type testSuiteRun struct {
	UUID        string `json:"uuid"`
	Status      string `json:"status"`
	PassedCount int    `json:"passed_count"`
	FailedCount int    `json:"failed_count"`
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
		} else if results[i].run.FailedCount > 0 {
			fmt.Printf("FAIL (%d/%d)\n", results[i].run.FailedCount, results[i].run.TotalCount)
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
		case r.run.FailedCount > 0:
			fmt.Printf("  FAIL  %s — %d/%d cases failed\n", r.name, r.run.FailedCount, r.run.TotalCount)
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

// pollUntilDone polls the run until it reaches a terminal status or the timeout
// expires.
func pollUntilDone(c *provisionClient, orgID uint, runUUID string, timeout time.Duration, out *testSuiteRun) error {
	deadline := time.Now().Add(timeout)
	for {
		body, status, err := c.getForOrg("/test-runs/"+runUUID, orgID)
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
