package commands

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(b bool) *bool { return &b }

func mustUnmarshalYAML(t *testing.T, src string, out any) {
	t.Helper()
	if err := yaml.Unmarshal([]byte(src), out); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
}

// TestPlaybookPayload_InternalOmittedWhenAbsent is the one that matters for
// every existing customer repo. The field is a pointer precisely so that a
// YAML file which says nothing about `internal` leaves the live value alone;
// a plain bool would send false on every apply and silently un-hide a
// building block someone had hidden.
func TestPlaybookPayload_InternalOmittedWhenAbsent(t *testing.T) {
	cfg := playbookConfig{Name: "Offerte Builder"}

	create := playbookCreatePayload(cfg)
	if _, present := create["internal"]; present {
		t.Errorf("create payload must omit internal when the YAML does not set it, got %+v", create)
	}

	update := playbookUpdatePayload(cfg, nil)
	if _, present := update["internal"]; present {
		t.Errorf("update payload must omit internal when the YAML does not set it, got %+v", update)
	}
}

func TestPlaybookPayload_InternalTrueIsSent(t *testing.T) {
	cfg := playbookConfig{Name: "Onderdeel: fotocontrole", Internal: boolPtr(true)}

	if got := playbookCreatePayload(cfg)["internal"]; got != true {
		t.Errorf("create payload internal = %v, want true", got)
	}
	if got := playbookUpdatePayload(cfg, nil)["internal"]; got != true {
		t.Errorf("update payload internal = %v, want true", got)
	}
}

// TestPlaybookPayload_InternalFalseIsSent: an explicit `internal: false` must
// reach the server, so un-hiding a playbook by editing its YAML works. This is
// the case a `len()`-style check or a non-pointer field would silently drop.
func TestPlaybookPayload_InternalFalseIsSent(t *testing.T) {
	cfg := playbookConfig{Name: "Offerte Builder", Internal: boolPtr(false)}

	got, present := playbookUpdatePayload(cfg, nil)["internal"]
	if !present {
		t.Fatal("an explicit internal: false must be sent, not omitted")
	}
	if got != false {
		t.Errorf("internal = %v, want false", got)
	}
}

// TestPlaybookYAML_InternalParses pins the YAML key name. A typo here would
// parse into a nil pointer and be omitted, i.e. fail silently — the same shape
// of bug as nav.yaml's default_route.
func TestPlaybookYAML_InternalParses(t *testing.T) {
	var cfg playbookConfig
	mustUnmarshalYAML(t, "name: Onderdeel\nslug: onderdeel\ninternal: true\n", &cfg)

	if cfg.Internal == nil {
		t.Fatal("internal: true did not parse; check the yaml tag")
	}
	if !*cfg.Internal {
		t.Errorf("internal parsed as %v, want true", *cfg.Internal)
	}
}
