package commands

import "testing"

// TestDiffPlaybookFields_InternalIsComparable is a regression test for a
// silent no-op shipped in the previous commit.
//
// diffPlaybookFields skips any payload key it cannot read back from the remote
// ("field we send but can't read back — can't diff it"), and apply short-
// circuits on an empty diff: `if diff.empty() { NOOP; return nil }`. So a YAML
// whose only change was `internal: true` diffed as NOOP, apply wrote nothing,
// and provision reported success while the playbook stayed visible.
func TestDiffPlaybookFields_InternalIsComparable(t *testing.T) {
	payload := map[string]interface{}{"name": "Onderdeel", "internal": true}
	remote := playbookDetailRemote{Name: "Onderdeel", Internal: false}

	changes := diffPlaybookFields(payload, remote)

	var found *fieldChange
	for i := range changes {
		if changes[i].Path == "internal" {
			found = &changes[i]
		}
	}
	if found == nil {
		t.Fatalf("internal must appear as a change so apply does not short-circuit to NOOP, got %+v", changes)
	}
	if found.Old != "false" || found.New != "true" {
		t.Errorf("internal change = %s -> %s, want false -> true", found.Old, found.New)
	}
}

// TestDiffPlaybookFields_InternalMatchingIsNotDrift: once the flag is applied,
// a re-run must settle back to NOOP rather than rewriting every time.
func TestDiffPlaybookFields_InternalMatchingIsNotDrift(t *testing.T) {
	payload := map[string]interface{}{"name": "Onderdeel", "internal": true}
	remote := playbookDetailRemote{Name: "Onderdeel", Internal: true}

	for _, c := range diffPlaybookFields(payload, remote) {
		if c.Path == "internal" {
			t.Errorf("matching internal must not be reported as drift, got %+v", c)
		}
	}
}

// TestDiffPlaybookFields_InternalAbsentFromYAMLIsNotDrift: the overwhelming
// majority of playbooks say nothing about internal. The payload then has no
// such key, so the field must never be compared and must never produce a
// write for an org that has not opted in.
func TestDiffPlaybookFields_InternalAbsentFromYAMLIsNotDrift(t *testing.T) {
	payload := map[string]interface{}{"name": "Offerte Builder"}
	remote := playbookDetailRemote{Name: "Offerte Builder", Internal: true}

	for _, c := range diffPlaybookFields(payload, remote) {
		if c.Path == "internal" {
			t.Errorf("internal must not be diffed when the YAML omits it, got %+v", c)
		}
	}
}
