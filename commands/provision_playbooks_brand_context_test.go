package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

// The regression these tests guard is not "brand_context is missing from the
// payload". It is that a single YAML file using a server-supported field this
// struct lacked failed the strict decode and aborted the ENTIRE provision run —
// every playbook, credential and dashboard in the directory, not just the file
// at fault. A directory containing one such playbook could not be applied at all.

// TestPlaybookConfig_BrandContextStrictDecode is the important one: yamlUnmarshalStrict
// uses KnownFields(true), so an unmodelled field is a hard error rather than a
// silently ignored key.
func TestPlaybookConfig_BrandContextStrictDecode(t *testing.T) {
	src := `
name: branded-image-playbook
trigger_type: manual
output_key: image
brand_context:
    knowledge_tag_slugs:
        - brand-logo
        - brand-palette
    text_key: brand_guidelines
    image_key: brand_reference_image
steps:
  - name: Generate
    step_type: generate_image
    output_key: image
`
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte(src), &cfg); err != nil {
		t.Fatalf("strict decode failed — this is the bug that took whole provision runs down: %v", err)
	}

	if cfg.BrandContext == nil {
		t.Fatal("brand_context decoded to nil; the field parsed but was dropped")
	}
	if got, want := len(cfg.BrandContext.KnowledgeTagSlugs), 2; got != want {
		t.Errorf("knowledge_tag_slugs len = %d, want %d", got, want)
	}
	if got, want := cfg.BrandContext.KnowledgeTagSlugs[0], "brand-logo"; got != want {
		t.Errorf("knowledge_tag_slugs[0] = %q, want %q", got, want)
	}
	if got, want := cfg.BrandContext.TextKey, "brand_guidelines"; got != want {
		t.Errorf("text_key = %q, want %q", got, want)
	}
	if got, want := cfg.BrandContext.ImageKey, "brand_reference_image"; got != want {
		t.Errorf("image_key = %q, want %q", got, want)
	}
}

// TestPlaybookConfig_NoBrandContextStaysNil confirms the field is genuinely
// optional. Every playbook that does not generate images omits it, so a
// non-nil zero value here would start sending brand_context on all of them.
func TestPlaybookConfig_NoBrandContextStaysNil(t *testing.T) {
	var cfg playbookConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\ntrigger_type: manual\n"), &cfg); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if cfg.BrandContext != nil {
		t.Errorf("BrandContext = %+v, want nil when the key is absent", cfg.BrandContext)
	}
}

// TestPlaybookPayloads_CarryBrandContext checks both wire directions. The json
// tags must match the server's PlaybookBrandContext field names exactly, so this
// asserts on the marshalled bytes rather than on the Go struct.
func TestPlaybookPayloads_CarryBrandContext(t *testing.T) {
	cfg := playbookConfig{
		Name:        "branded",
		TriggerType: "manual",
		BrandContext: &playbookBrandContext{
			KnowledgeTagSlugs: []string{"brand-logo"},
			TextKey:           "brand_guidelines",
			ImageKey:          "brand_reference_image",
		},
	}

	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", playbookCreatePayload(cfg)},
		{"update", playbookUpdatePayload(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(raw)
			for _, want := range []string{
				`"brand_context"`,
				`"knowledge_tag_slugs":["brand-logo"]`,
				`"text_key":"brand_guidelines"`,
				`"image_key":"brand_reference_image"`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("payload missing %s\n  got: %s", want, got)
				}
			}
		})
	}
}

// TestPlaybookPayloads_OmitBrandContextWhenAbsent guards the other direction.
// The server applies brand_context only when the key is non-nil, so sending it
// unconditionally would let an apply clobber brand config set elsewhere on
// every playbook that does not declare one.
func TestPlaybookPayloads_OmitBrandContextWhenAbsent(t *testing.T) {
	cfg := playbookConfig{Name: "plain", TriggerType: "manual"}

	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", playbookCreatePayload(cfg)},
		{"update", playbookUpdatePayload(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, present := tc.payload["brand_context"]; present {
				t.Errorf("brand_context present in %s payload when the YAML declared none", tc.name)
			}
		})
	}
}
