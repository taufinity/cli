package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

// provision decodes YAML with KnownFields(true), so a widget declaring a field
// the struct lacks fails the strict decode and takes the whole run down. This
// pins agent_enabled_tools as a known key, and pins the nil-vs-empty contract:
// absent stays nil (leave the server value alone), an explicit empty list
// decodes non-nil (clear it).
func TestWidgetConfig_AgentEnabledToolsStrictDecode(t *testing.T) {
	src := `
name: Support widget
agent_enabled_tools:
  - image_gen
  - web_search
`
	var cfg widgetConfig
	if err := yamlUnmarshalStrict([]byte(src), &cfg); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if got, want := len(cfg.AgentEnabledTools), 2; got != want {
		t.Fatalf("agent_enabled_tools = %v, want %d entries", cfg.AgentEnabledTools, want)
	}

	var absent widgetConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\n"), &absent); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if absent.AgentEnabledTools != nil {
		t.Errorf("AgentEnabledTools = %v, want nil when the key is absent", absent.AgentEnabledTools)
	}

	var empty widgetConfig
	if err := yamlUnmarshalStrict([]byte("name: plain\nagent_enabled_tools: []\n"), &empty); err != nil {
		t.Fatalf("strict decode failed: %v", err)
	}
	if empty.AgentEnabledTools == nil {
		t.Error("AgentEnabledTools = nil for an explicit empty list, want non-nil so the payload clears the field")
	}
}

func TestWidgetPayloads_CarryAgentEnabledTools(t *testing.T) {
	cfg := widgetConfig{Name: "support", AgentEnabledTools: []string{"image_gen", "web_search"}}

	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", widgetCreatePayload(cfg)},
		{"update", widgetUpdatePayload(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Assert on the wire bytes: the field must go out as a JSON array
			// (same shape as agent_mcp_tools), not a joined string.
			if want := `"agent_enabled_tools":["image_gen","web_search"]`; !strings.Contains(string(raw), want) {
				t.Errorf("payload missing %s\n  got: %s", want, raw)
			}
		})
	}
}

func TestWidgetPayloads_OmitAgentEnabledToolsWhenAbsent(t *testing.T) {
	cfg := widgetConfig{Name: "plain"}
	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", widgetCreatePayload(cfg)},
		{"update", widgetUpdatePayload(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, present := tc.payload["agent_enabled_tools"]; present {
				t.Errorf("agent_enabled_tools present in %s payload when the YAML declared none", tc.name)
			}
		})
	}
}

func TestWidgetPayloads_EmptyAgentEnabledToolsExplicitlyClears(t *testing.T) {
	cfg := widgetConfig{Name: "support", AgentEnabledTools: []string{}}
	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
	}{
		{"create", widgetCreatePayload(cfg)},
		{"update", widgetUpdatePayload(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if want := `"agent_enabled_tools":[]`; !strings.Contains(string(raw), want) {
				t.Errorf("payload missing %s (empty list must be sent to clear the server value)\n  got: %s", want, raw)
			}
		})
	}
}
