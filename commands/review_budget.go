package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/taufinity/cli/internal/api"
)

// reviewSettingsPath is the member view of the organisation's code review
// settings. It carries percentages only; the budget line below is built from
// exactly two of its fields, so nothing else in the answer can reach the
// terminal, an amount in USD least of all: members see percent of budget,
// amounts are for system admins in Studio.
const reviewSettingsPath = "/api/code-review/settings"

// reviewBudgetLine asks Studio how much of the budget is used and returns the
// line to print, or "" when the call fails or the answer has no percentages.
// A failure must not change the outcome of the review, so it is silent.
func reviewBudgetLine(ctx context.Context, client *api.Client) string {
	resp, err := client.GetWithKeyOrAuth(ctx, reviewSettingsPath)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	var s struct {
		Daily   *float64 `json:"daily_pct_used"`
		Monthly *float64 `json:"monthly_pct_used"`
	}
	if err := json.Unmarshal(resp.Body, &s); err != nil || s.Daily == nil || s.Monthly == nil {
		return ""
	}
	return fmt.Sprintf("Code review budget used: %s%% today, %s%% this month", pct(*s.Daily), pct(*s.Monthly))
}

// pct renders a percentage without a trailing ".0".
func pct(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
