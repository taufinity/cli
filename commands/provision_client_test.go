package commands

import (
	"strings"
	"testing"
)

// TestProvisionSummarize_MasksSensitiveValues is the regression test for a
// real secrets-leak found live 2026-08-06: dry-run/error output printed the
// first 120 bytes of the raw request payload with zero redaction. A
// credential upsert's payload — {"name":"odoo-taufinity","values_json":
// "{\"api_key\":\"...\"}"}  — puts the actual secret value right at the
// start, so it landed in plain terminal output during a routine `provision
// diff`. provisionSummarize is shared by every provisioner (not just
// credentials), so the fix is generic: mask any JSON key whose name looks
// secret-shaped, regardless of which provisioner's payload it's in.
func TestProvisionSummarize_MasksSensitiveValues(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		mustNot string // substring that must NOT appear in the output
		must    string // substring that MUST appear (proves masking happened, not silent drop)
	}{
		{
			name:    "credential values_json",
			payload: `{"name":"odoo-taufinity","values_json":"{\"api_key\":\"c6a55f7de4a21c42944d0d443ee500103e2763f7\"}"}`,
			mustNot: "c6a55f7de4a21c42944d0d443ee500103e2763f7",
			must:    "REDACTED",
		},
		{
			name:    "webhook url",
			payload: `{"name":"slack-support","values_json":"{\"default_url\":\"https://hooks.slack.com/services/T09QU7G7SC9/B0AGXQ7HF3P/DhGTO\"}"}`,
			mustNot: "hooks.slack.com/services",
			must:    "REDACTED",
		},
		{
			name:    "generic password field",
			payload: `{"username":"admin","password":"hunter2"}`,
			mustNot: "hunter2",
			must:    "REDACTED",
		},
		{
			name:    "generic token field",
			payload: `{"api_token":"sk-abc123"}`,
			mustNot: "sk-abc123",
			must:    "REDACTED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := provisionSummarize([]byte(tc.payload))
			if strings.Contains(got, tc.mustNot) {
				t.Errorf("output still contains the secret value: %s", got)
			}
			if !strings.Contains(got, tc.must) {
				t.Errorf("output = %q, want it to contain %q (proves masking, not silent drop)", got, tc.must)
			}
		})
	}
}

// TestProvisionSummarize_PreservesNonSensitiveFields confirms the fix is a
// mask, not a blanket redaction — dry-run output is only useful if
// non-sensitive fields (what's actually being changed) stay visible.
func TestProvisionSummarize_PreservesNonSensitiveFields(t *testing.T) {
	got := provisionSummarize([]byte(`{"agent_tool_policy":["web_search","notify_human"],"name":"taufinity-org"}`))
	if !strings.Contains(got, "notify_human") {
		t.Errorf("non-sensitive value was masked or dropped: %s", got)
	}
	if !strings.Contains(got, "taufinity-org") {
		t.Errorf("non-sensitive value was masked or dropped: %s", got)
	}
}

// TestProvisionSummarize_NonJSONPayloadStillTruncates confirms the fallback
// path (payload that isn't valid JSON) still works — the multipart upload
// dry-run path and any malformed body must not panic or error out.
func TestProvisionSummarize_NonJSONPayloadStillTruncates(t *testing.T) {
	got := provisionSummarize([]byte("not json at all"))
	if got != "not json at all" {
		t.Errorf("got %q, want the raw string unchanged (short, non-JSON payload)", got)
	}
}

// TestProvisionSummarize_TruncatesLongOutput confirms the 120-char cap still
// applies after masking.
func TestProvisionSummarize_TruncatesLongOutput(t *testing.T) {
	long := `{"description":"` + strings.Repeat("a", 300) + `"}`
	got := provisionSummarize([]byte(long))
	if len(got) > 130 { // 120 + "..." + a little JSON overhead slack
		t.Errorf("output length = %d, want it capped near 120 chars, got: %s", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated output to end with '...', got: %s", got)
	}
}
