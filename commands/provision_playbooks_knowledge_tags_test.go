package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

// playbook_knowledge_tags decides what every ai_prompt step in a playbook can
// see. Leaving it out of provision meant that switch lived only in the Studio
// UI — not in version control, not reviewable, and silently different between
// environments.

func TestPlaybookConfig_KnowledgeTagsStrictDecode(t *testing.T) {
	src := `
name: MT Deck
trigger_type: schedule
knowledge_tags:
  - mt-deck
  - mt-meetings-rollup
`
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte(src), &cfg); err != nil {
		t.Fatalf("strict decode failed — an unmodelled field takes the whole provision run down: %v", err)
	}
	if cfg.KnowledgeTags == nil {
		t.Fatal("knowledge_tags decoded to nil; the field parsed but was dropped")
	}
	if got, want := len(*cfg.KnowledgeTags), 2; got != want {
		t.Fatalf("knowledge_tags len = %d, want %d", got, want)
	}
	if got := (*cfg.KnowledgeTags)[0]; got != "mt-deck" {
		t.Errorf("knowledge_tags[0] = %q, want %q", got, "mt-deck")
	}
}

// Absent must stay nil. The server replaces the whole association set when the
// key is present, so sending an empty list where the YAML said nothing would
// silently detach every tag — turning "I didn't mention it" into "remove them
// all", and quietly starving the playbook's prompts of context.
func TestPlaybookConfig_KnowledgeTagsAbsentStaysNil(t *testing.T) {
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\ntrigger_type: manual\n"), &cfg); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if cfg.KnowledgeTags != nil {
		t.Errorf("KnowledgeTags = %v, want nil when the key is absent", *cfg.KnowledgeTags)
	}
}

// An explicitly empty list is a real instruction ("detach everything") and must
// survive as non-nil, distinct from absent.
func TestPlaybookConfig_KnowledgeTagsEmptyIsExplicit(t *testing.T) {
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\nknowledge_tags: []\n"), &cfg); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if cfg.KnowledgeTags == nil {
		t.Fatal("explicit empty list decoded to nil — indistinguishable from absent")
	}
	if got := len(*cfg.KnowledgeTags); got != 0 {
		t.Errorf("len = %d, want 0", got)
	}
}

func TestPlaybookUpdatePayload_CarriesKnowledgeTagIDs(t *testing.T) {
	cfg := playbookConfig{Name: "MT Deck", TriggerType: "schedule"}
	raw, err := json.Marshal(playbookUpdatePayload(cfg, []uint{39, 1655}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Assert on the wire bytes: the json key must match the server's field name,
	// which the Go parameter name does not guarantee.
	if want := `"knowledge_tag_ids":[39,1655]`; !strings.Contains(string(raw), want) {
		t.Errorf("payload missing %s\n  got: %s", want, raw)
	}
}

// nil ids must omit the key entirely, so an apply that says nothing about tags
// leaves the live associations alone.
func TestPlaybookUpdatePayload_OmitsKnowledgeTagIDsWhenNil(t *testing.T) {
	cfg := playbookConfig{Name: "plain", TriggerType: "manual"}
	if _, present := playbookUpdatePayload(cfg, nil)["knowledge_tag_ids"]; present {
		t.Error("knowledge_tag_ids present when no tags were resolved — this would detach live tags")
	}
}

// An explicit empty slice is NOT nil and must be sent, so "detach all" reaches
// the server.
func TestPlaybookUpdatePayload_SendsEmptyKnowledgeTagIDs(t *testing.T) {
	cfg := playbookConfig{Name: "plain", TriggerType: "manual"}
	got, present := playbookUpdatePayload(cfg, []uint{})["knowledge_tag_ids"]
	if !present {
		t.Fatal("explicit empty list was dropped — 'detach all' would never reach the server")
	}
	if ids, ok := got.([]uint); !ok || len(ids) != 0 {
		t.Errorf("knowledge_tag_ids = %v, want an empty []uint", got)
	}
}
