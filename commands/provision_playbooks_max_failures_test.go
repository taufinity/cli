package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

// The regression here is not "the field is missing from the payload". It is that
// provision decodes YAML with KnownFields(true), so a playbook declaring a field
// this struct lacks fails the strict decode and takes the ENTIRE run down —
// every playbook, credential and dashboard in the directory, not just the file
// at fault. Third field to hit this (after agent_input_schema and brand_context).

func TestPlaybookConfig_MaxConsecutiveFailuresStrictDecode(t *testing.T) {
	src := `
name: Weekly digest
trigger_type: schedule
schedule: "0 9 * * 1"
max_consecutive_failures: 1
output_key: digest
`
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte(src), &cfg); err != nil {
		t.Fatalf("strict decode failed — this is the bug that takes whole provision runs down: %v", err)
	}
	if cfg.MaxConsecutiveFailures == nil {
		t.Fatal("max_consecutive_failures decoded to nil; the field parsed but was dropped")
	}
	if got := *cfg.MaxConsecutiveFailures; got != 1 {
		t.Errorf("max_consecutive_failures = %d, want 1", got)
	}
}

// Absent must stay nil so the server's default of 3 is left alone. A non-nil
// zero value would send 0, and the server only applies values > 0 — so it would
// be silently ignored while looking deliberate in the YAML.
func TestPlaybookConfig_MaxConsecutiveFailuresAbsentStaysNil(t *testing.T) {
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\ntrigger_type: manual\n"), &cfg); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if cfg.MaxConsecutiveFailures != nil {
		t.Errorf("MaxConsecutiveFailures = %v, want nil when the key is absent", *cfg.MaxConsecutiveFailures)
	}
}

func TestPlaybookPayloads_CarryMaxConsecutiveFailures(t *testing.T) {
	one := 1
	cfg := playbookConfig{Name: "weekly", TriggerType: "schedule", MaxConsecutiveFailures: &one}

	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", playbookCreatePayload(cfg)},
		{"update", playbookUpdatePayload(cfg, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Assert on the wire bytes: the json key has to match the server's
			// field name exactly, which the Go struct name does not guarantee.
			if want := `"max_consecutive_failures":1`; !strings.Contains(string(raw), want) {
				t.Errorf("payload missing %s\n  got: %s", want, raw)
			}
		})
	}
}

func TestPlaybookPayloads_OmitMaxConsecutiveFailuresWhenAbsent(t *testing.T) {
	cfg := playbookConfig{Name: "plain", TriggerType: "manual"}
	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", playbookCreatePayload(cfg)},
		{"update", playbookUpdatePayload(cfg, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, present := tc.payload["max_consecutive_failures"]; present {
				t.Errorf("max_consecutive_failures present in %s payload when the YAML declared none", tc.name)
			}
		})
	}
}
