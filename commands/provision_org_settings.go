// provision_org_settings.go — provision support for org-level settings via
// <dir>/org-settings.yaml → PUT /api/organizations/{id}/settings.
//
// Mirrors the full field set of api/handlers/organization_settings.go's
// UpdateOrganizationSettingsRequest (ai-site-gen) — a flat, partial-update
// body: only fields present in the YAML are sent, everything else is left
// untouched server-side. This is a different endpoint from
// PATCH /organizations/{id} (agent_tool_policy, studio_api_allowlist —
// see provision_agent_tools.go); org-level config is split across the two
// REST resources upstream, so provision mirrors that split rather than
// merging them into one YAML file.
//
// default_knowledge_tags is the field this was built for: the org-wide
// fallback knowledge tags that chat widgets, playbooks, and playbook steps
// use for RAG context injection when they have no explicit tag
// configuration of their own (see internal/knowledge's
// ResolveKnowledgeTagsWithOrgDefault). Empty/unset means no fallback
// injection happens at all.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// orgSettingsConfig mirrors UpdateOrganizationSettingsRequest field-for-field
// (api/handlers/organization_settings.go). Pointer/nil fields are omitted
// from the outgoing JSON so the API's partial-update semantics are
// preserved — a field absent from the YAML must never appear in the
// request body, or it would silently clear a setting nobody asked to touch.
type orgSettingsConfig struct {
	PrivacyProtectionEnabled *bool `yaml:"privacy_protection_enabled,omitempty" json:"privacy_protection_enabled,omitempty"`
	ChatLogEnabled           *bool `yaml:"chat_log_enabled,omitempty"           json:"chat_log_enabled,omitempty"`
	DefaultHomepage          *string `yaml:"default_homepage,omitempty" json:"default_homepage,omitempty"`
	SidebarBg                *string `yaml:"sidebar_bg,omitempty"         json:"sidebar_bg,omitempty"`
	SidebarHoverBg           *string `yaml:"sidebar_hover_bg,omitempty"   json:"sidebar_hover_bg,omitempty"`
	SidebarText              *string `yaml:"sidebar_text,omitempty"       json:"sidebar_text,omitempty"`
	SidebarTextMuted         *string `yaml:"sidebar_text_muted,omitempty" json:"sidebar_text_muted,omitempty"`
	SidebarBorder            *string `yaml:"sidebar_border,omitempty"     json:"sidebar_border,omitempty"`
	ContentBg                *string `yaml:"content_bg,omitempty"         json:"content_bg,omitempty"`
	AllowedEmailDomains      *string `yaml:"allowed_email_domains,omitempty" json:"allowed_email_domains,omitempty"`
	RequirePortalDomain      *bool   `yaml:"require_portal_domain,omitempty" json:"require_portal_domain,omitempty"`
	// System-admin-only fields upstream — provision always authenticates as
	// the bootstrap admin key, so these are reachable here, but treat them
	// with the same care the API docs call for (allowlist restricts
	// credential_inject targets; the timeout raises a real execution ceiling).
	AllowedCredentialInjectDomains *string `yaml:"allowed_credential_inject_domains,omitempty" json:"allowed_credential_inject_domains,omitempty"`
	AgentRunWaitTimeoutSecondsMax  *int    `yaml:"agent_run_wait_timeout_seconds_max,omitempty" json:"agent_run_wait_timeout_seconds_max,omitempty"`
	// DefaultKnowledgeTags is a YAML list here (ergonomic to author/diff one
	// tag per concern) but the API wants a single comma-separated string —
	// joined in applyOrgSettings, not in this struct, so the struct stays a
	// faithful field-for-field mirror of the API's own request shape.
	DefaultKnowledgeTags []string `yaml:"default_knowledge_tags,omitempty" json:"-"`
}

// applyOrgSettings reads <dir>/org-settings.yaml and PUTs whatever fields
// are present to /organizations/{id}/settings. Missing file is a no-op
// (opt-in, like every other settings provisioner). A YAML file that sets no
// recognized field is an error, matching the API's own "no settings
// provided to update" rejection — surfacing the mistake at provision time
// instead of a 400 from the server with less context.
func applyOrgSettings(c *provisionClient, dir string, orgID uint) error {
	path := filepath.Join(dir, "org-settings.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read org-settings.yaml: %w", err)
	}

	var cfg orgSettingsConfig
	if err := yamlUnmarshalStrict(data, &cfg); err != nil {
		return fmt.Errorf("parse org-settings.yaml: %w", err)
	}

	body := map[string]any{}
	if cfg.PrivacyProtectionEnabled != nil {
		body["privacy_protection_enabled"] = *cfg.PrivacyProtectionEnabled
	}
	if cfg.ChatLogEnabled != nil {
		body["chat_log_enabled"] = *cfg.ChatLogEnabled
	}
	if cfg.DefaultHomepage != nil {
		body["default_homepage"] = *cfg.DefaultHomepage
	}
	if cfg.SidebarBg != nil {
		body["sidebar_bg"] = *cfg.SidebarBg
	}
	if cfg.SidebarHoverBg != nil {
		body["sidebar_hover_bg"] = *cfg.SidebarHoverBg
	}
	if cfg.SidebarText != nil {
		body["sidebar_text"] = *cfg.SidebarText
	}
	if cfg.SidebarTextMuted != nil {
		body["sidebar_text_muted"] = *cfg.SidebarTextMuted
	}
	if cfg.SidebarBorder != nil {
		body["sidebar_border"] = *cfg.SidebarBorder
	}
	if cfg.ContentBg != nil {
		body["content_bg"] = *cfg.ContentBg
	}
	if cfg.AllowedEmailDomains != nil {
		body["allowed_email_domains"] = *cfg.AllowedEmailDomains
	}
	if cfg.RequirePortalDomain != nil {
		body["require_portal_domain"] = *cfg.RequirePortalDomain
	}
	if cfg.AllowedCredentialInjectDomains != nil {
		body["allowed_credential_inject_domains"] = *cfg.AllowedCredentialInjectDomains
	}
	if cfg.AgentRunWaitTimeoutSecondsMax != nil {
		body["agent_run_wait_timeout_seconds_max"] = *cfg.AgentRunWaitTimeoutSecondsMax
	}
	if cfg.DefaultKnowledgeTags != nil {
		body["default_knowledge_tags"] = strings.Join(cfg.DefaultKnowledgeTags, ",")
	}

	if len(body) == 0 {
		return fmt.Errorf("org-settings.yaml: no recognized settings fields set")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("org-settings.yaml: marshal payload: %w", err)
	}

	apiPath := fmt.Sprintf("/organizations/%d/settings", orgID)
	respBody, status, err := c.put(apiPath, payload)
	if err != nil || status >= 300 {
		return provisionAPIErr(fmt.Sprintf("update org %d settings", orgID), status, respBody, err)
	}

	if c.dryRun {
		return nil
	}
	fmt.Printf("provision: org %d settings updated (%d field(s))\n", orgID, len(body))
	return nil
}
