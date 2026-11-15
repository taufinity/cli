package commands

import (
	"testing"

	"github.com/taufinity/cli/internal/review"
)

// A request_changes verdict means the service saw a critical or high finding.
// When the result carries none, the counts were lost on the way (production,
// 2026-11-12) and --fail-on must fail rather than pass.
func TestExceedsFailOnRequestChangesWithoutCounts(t *testing.T) {
	zero := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	tests := map[string]struct {
		t      review.Terminal
		failOn string
		want   bool
	}{
		"zero_counts_critical":         {review.Terminal{Verdict: "request_changes", Counts: zero}, "critical", true},
		"zero_counts_high":             {review.Terminal{Verdict: "request_changes", Counts: zero}, "high", true},
		"zero_counts_medium":           {review.Terminal{Verdict: "request_changes", Counts: zero}, "medium", true},
		"missing_counts":               {review.Terminal{Verdict: "request_changes"}, "critical", true},
		"only_low_counted":             {review.Terminal{Verdict: "request_changes", Counts: map[string]int{"low": 2}}, "critical", true},
		"none_never_fails":             {review.Terminal{Verdict: "request_changes", Counts: zero}, "none", false},
		"high_explains_the_verdict":    {review.Terminal{Verdict: "request_changes", Counts: map[string]int{"high": 1}}, "critical", false},
		"high_finding_explains":        {review.Terminal{Verdict: "request_changes", Findings: []review.Finding{{Severity: "High"}}}, "critical", false},
		"comment_with_zero_counts":     {review.Terminal{Verdict: "comment", Counts: zero}, "critical", false},
		"approve_with_zero_counts":     {review.Terminal{Verdict: "approve", Counts: zero}, "critical", false},
		"critical_counted_still_fails": {review.Terminal{Verdict: "request_changes", Counts: map[string]int{"critical": 1}}, "critical", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := exceedsFailOn(tc.t, tc.failOn); got != tc.want {
				t.Errorf("exceedsFailOn = %v, want %v", got, tc.want)
			}
		})
	}
}
