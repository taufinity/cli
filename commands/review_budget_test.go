package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewCostDisplay(t *testing.T) {
	result := readFixture(t, "stream_result.ndjson")
	if !strings.Contains(result, `"cost_usd":0.4213`) {
		t.Fatal("the fixture should carry a cost")
	}
	forbidden := []string{"0.42", "0.4213", "$", "USD", "usd", "cost_usd"}

	t.Run("no_usd_anywhere", func(t *testing.T) {
		for _, args := range [][]string{{"review"}, {"review", "--json"}, {"review", "--fail-on", "high"}} {
			useAPIKey(t)
			f := newFakeStudio(t, result)
			out, errOut, _ := runReviewIn(t, newReviewTestRepo(t), append([]string{"--api-url", f.srv.URL}, args...)...)
			for _, bad := range forbidden {
				if strings.Contains(out, bad) || strings.Contains(errOut, bad) {
					t.Errorf("%v: output contains %q\nstdout=%s\nstderr=%s", args, bad, out, errOut)
				}
			}
		}
	})

	t.Run("budget_percent_shown", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut, "Code review budget used: 6% today, 2% this month") {
			t.Errorf("stderr:\n%s", errOut)
		}
	})

	t.Run("settings_failure_is_silent", func(t *testing.T) {
		for _, code := range []int{403, 500} {
			useAPIKey(t)
			f := newFakeStudio(t, result)
			f.settingsCode = code
			f.settingsBody = `{"error":"forbidden"}`
			_, errOut, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--fail-on", "high")
			if exitCode(err) != 1 {
				t.Errorf("status %d: exit %d, the findings decide, want 1", code, exitCode(err))
			}
			if strings.Contains(strings.ToLower(errOut), "budget") || strings.Contains(errOut, "forbidden") {
				t.Errorf("status %d: a failing settings call must print nothing about budget:\n%s", code, errOut)
			}
		}
	})

	t.Run("settings_without_percentages_is_silent", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		f.settingsBody = `{"organization_id":12,"enabled":true}`
		_, errOut, _ := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if strings.Contains(strings.ToLower(errOut), "budget") {
			t.Errorf("stderr:\n%s", errOut)
		}
	})

	t.Run("no_budget_call_after_an_error_line", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, readFixture(t, "stream_error.ndjson"))
		_, _, _ = runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review")
		if len(f.requests()) != 1 {
			t.Errorf("requests %d", len(f.requests()))
		}
	})

	t.Run("json_strips_cost_usd", func(t *testing.T) {
		useAPIKey(t)
		f := newFakeStudio(t, result)
		out, _, err := runReviewIn(t, newReviewTestRepo(t), "--api-url", f.srv.URL, "review", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["cost_usd"]; ok || got["verdict"] != "request_changes" {
			t.Errorf("terminal line: %v", got)
		}
	})
}
