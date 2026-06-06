// provision_widgets.go — provision support for chat widget resources.
//
// Pattern mirrors provision_playbooks.go: list, match by name, upsert. Widgets do not
// have child rows in our model, so reconciliation is single-call.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// widgetConfig is the YAML shape for a chat widget. Includes intake
// questionnaire support for WVS use cases.
type widgetConfig struct {
	Name                      string   `yaml:"name"`
	Description               string   `yaml:"description,omitempty"`
	Enabled                   *bool    `yaml:"enabled,omitempty"`
	SystemPrompt              string   `yaml:"system_prompt,omitempty"`
	FirstMessage              string   `yaml:"first_message,omitempty"`
	AIProvider                string   `yaml:"ai_provider,omitempty"`
	AIModel                   string   `yaml:"ai_model,omitempty"`
	MaxTokens                 int      `yaml:"max_tokens,omitempty"`
	Temperature               float64  `yaml:"temperature,omitempty"`
	RateLimitPerMin           int      `yaml:"rate_limit_per_min,omitempty"`
	AllowAnonymous            *bool    `yaml:"allow_anonymous,omitempty"`
	AllowedOrigins            string   `yaml:"allowed_origins,omitempty"`
	DefaultLanguage           string   `yaml:"default_language,omitempty"`
	AllowedLanguages          []string `yaml:"allowed_languages,omitempty"`
	GoalsDescription          string   `yaml:"goals_description,omitempty"`
	IntakeQuestionsJSON       string   `yaml:"intake_questions_json,omitempty"`
	LLMValidationEnabled      *bool    `yaml:"llm_validation_enabled,omitempty"`
	PageAwarenessEnabled      *bool    `yaml:"page_awareness_enabled,omitempty"`
	KnowledgeSourcesEnabled   *bool    `yaml:"knowledge_sources_enabled,omitempty"`
	PassthroughSystemMessages *bool    `yaml:"passthrough_system_messages,omitempty"`
	AgentEnabled              *bool    `yaml:"agent_enabled,omitempty"`
	AgentMCPServer            string   `yaml:"agent_mcp_server,omitempty"`
	AgentMCPTools             []string `yaml:"agent_mcp_tools,omitempty"`
	// RouterName links the widget to an existing router (matched by name).
	RouterName string `yaml:"router_name,omitempty"`
	// Slug is a stable, human-readable identifier for the widget within the org.
	Slug                  string   `yaml:"slug,omitempty"`
	FileUploadEnabled     *bool    `yaml:"file_upload_enabled,omitempty"`
	AllowedMimeTypes      []string `yaml:"allowed_mime_types,omitempty"`
	MaxFileSizeMB         int      `yaml:"max_file_size_mb,omitempty"`
	AudioRecordingEnabled *bool    `yaml:"audio_recording_enabled,omitempty"`
}

// widgetListItem is the minimal shape from GET /api/widgets/.
type widgetListItem struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug,omitempty"`
	RouterID *uint  `json:"router_id,omitempty"`
}

// applyWidgets applies all widget YAML files from dir to the org.
// Supports a single widget.yaml at the root and/or a widgets/ subdirectory.
func applyWidgets(c *provisionClient, dir string, orgID uint) error {
	// single widget.yaml at root
	if wf := filepath.Join(dir, "widget.yaml"); fileExists(wf) {
		var cfg widgetConfig
		mustReadYAML(wf, &cfg)
		if err := upsertWidget(c, orgID, cfg); err != nil {
			return fmt.Errorf("widget: %w", err)
		}
	}
	// multi-widget directory
	wd := filepath.Join(dir, "widgets")
	if fileExists(wd) {
		entries, err := os.ReadDir(wd)
		if err != nil {
			return fmt.Errorf("read widgets dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			wf := filepath.Join(wd, e.Name())
			var cfg widgetConfig
			mustReadYAML(wf, &cfg)
			if err := upsertWidget(c, orgID, cfg); err != nil {
				return fmt.Errorf("widget %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// upsertWidget creates or updates a widget for the given org.
func upsertWidget(c *provisionClient, orgID uint, cfg widgetConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("widget: name is required")
	}

	body, status, err := c.getForOrg("/widgets/", orgID)
	if err != nil || status != 200 {
		return fmt.Errorf("list widgets: status=%d err=%v body=%s", status, err, provisionSummarize(body))
	}
	var existing []widgetListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return fmt.Errorf("parse widgets: %w (body=%s)", err, provisionSummarize(body))
	}

	// Slug-first matching: when the YAML provides a slug, match by slug so that
	// the display name can be freely changed without creating a duplicate widget.
	var matches []widgetListItem
	if cfg.Slug != "" {
		for _, w := range existing {
			if strings.EqualFold(w.Slug, cfg.Slug) {
				matches = append(matches, w)
			}
		}
		if len(matches) == 0 {
			// Also check by name in case the widget predates slug support.
			for _, w := range existing {
				if strings.EqualFold(w.Name, cfg.Name) && w.Slug == "" {
					matches = append(matches, w)
				}
			}
		}
	} else {
		for _, w := range existing {
			if strings.EqualFold(w.Name, cfg.Name) {
				matches = append(matches, w)
			}
		}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("id=%d", m.ID))
		}
		return fmt.Errorf("widget %q: %d matches in org %d (%s) — ambiguous, refusing to apply",
			cfg.Name, len(matches), orgID, strings.Join(ids, ", "))
	}

	if len(matches) == 1 {
		id := matches[0].ID
		fmt.Printf("UPDATE widget %q id=%d\n", cfg.Name, id)
		payload, _ := json.Marshal(widgetUpdatePayload(cfg))
		if _, status, err = c.writeForOrg("PUT", fmt.Sprintf("/widgets/%d", id), payload, orgID); err != nil || status >= 300 {
			return fmt.Errorf("update widget %q: status=%d err=%v", cfg.Name, status, err)
		}
		return linkWidgetRouter(c, orgID, id, cfg.RouterName)
	}

	fmt.Printf("CREATE widget %q\n", cfg.Name)
	payload, _ := json.Marshal(widgetCreatePayload(cfg))
	respBody, status, err := c.writeForOrg("POST", "/widgets/", payload, orgID)
	if err != nil || status >= 300 {
		// Skip-not-fail policy: a fresh org without AI provider credentials
		// configured returns 400 "AI credentials not configured for <provider>".
		if status == 400 && isCredentialsMissingError(respBody) {
			c.Warn("widget %q skipped: AI credentials not configured for this org. Open Studio → Settings → Secrets, add the provider key, then re-run provision.", cfg.Name)
			return nil
		}
		return fmt.Errorf("create widget %q: status=%d err=%v body=%s", cfg.Name, status, err, provisionSummarize(respBody))
	}
	var created widgetListItem
	if err := json.Unmarshal(respBody, &created); err != nil {
		return fmt.Errorf("parse created widget: %w", err)
	}
	return linkWidgetRouter(c, orgID, created.ID, cfg.RouterName)
}

// linkWidgetRouter resolves routerName → ID and sets router_id on the widget.
// No-op when routerName is empty.
func linkWidgetRouter(c *provisionClient, orgID, widgetID uint, routerName string) error {
	if strings.TrimSpace(routerName) == "" {
		return nil
	}
	body, status, err := c.getForOrg("/routers/", orgID)
	if err != nil || status != 200 {
		return fmt.Errorf("link router: list routers status=%d err=%v", status, err)
	}
	var routers []routerListItem
	if err := unmarshalListEnvelope(body, &routers); err != nil {
		return fmt.Errorf("link router: parse routers: %w", err)
	}
	var routerID uint
	for _, r := range routers {
		if strings.EqualFold(r.Name, routerName) {
			routerID = r.ID
			break
		}
	}
	if routerID == 0 {
		return fmt.Errorf("link router: router %q not found in org %d", routerName, orgID)
	}
	fmt.Printf("LINK widget id=%d router=%q id=%d\n", widgetID, routerName, routerID)
	payload, _ := json.Marshal(map[string]any{"router_id": routerID})
	if _, status, err = c.writeForOrg("PUT", fmt.Sprintf("/widgets/%d", widgetID), payload, orgID); err != nil || status >= 300 {
		return fmt.Errorf("link router: update widget %d: status=%d err=%v", widgetID, status, err)
	}
	return nil
}

// isCredentialsMissingError pattern-matches the well-known server message
// emitted when an org lacks AI credentials.
func isCredentialsMissingError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return strings.Contains(string(body), "AI credentials not configured")
}

// widgetCreatePayload mirrors the CreateRequest struct.
func widgetCreatePayload(cfg widgetConfig) map[string]interface{} {
	out := map[string]interface{}{
		"name":        cfg.Name,
		"description": cfg.Description,
	}
	if cfg.Slug != "" {
		out["slug"] = cfg.Slug
	}
	if cfg.SystemPrompt != "" {
		out["system_prompt"] = cfg.SystemPrompt
	}
	if cfg.FirstMessage != "" {
		out["first_message"] = cfg.FirstMessage
	}
	if cfg.AIProvider != "" {
		out["ai_provider"] = cfg.AIProvider
	}
	if cfg.AIModel != "" {
		out["ai_model"] = cfg.AIModel
	}
	if cfg.MaxTokens > 0 {
		out["max_tokens"] = cfg.MaxTokens
	}
	if cfg.Temperature > 0 {
		out["temperature"] = cfg.Temperature
	}
	if cfg.RateLimitPerMin > 0 {
		out["rate_limit_per_min"] = cfg.RateLimitPerMin
	}
	if cfg.AllowAnonymous != nil {
		out["allow_anonymous"] = *cfg.AllowAnonymous
	}
	if cfg.AllowedOrigins != "" {
		out["allowed_origins"] = cfg.AllowedOrigins
	}
	if cfg.DefaultLanguage != "" {
		out["default_language"] = cfg.DefaultLanguage
	}
	if len(cfg.AllowedLanguages) > 0 {
		out["allowed_languages"] = cfg.AllowedLanguages
	}
	if cfg.LLMValidationEnabled != nil {
		out["llm_validation_enabled"] = *cfg.LLMValidationEnabled
	}
	if cfg.PageAwarenessEnabled != nil {
		out["page_awareness_enabled"] = *cfg.PageAwarenessEnabled
	}
	if cfg.PassthroughSystemMessages != nil {
		out["passthrough_system_messages"] = *cfg.PassthroughSystemMessages
	}
	applyWidgetAgentFields(out, cfg)
	applyWidgetMediaFields(out, cfg)
	return out
}

// applyWidgetAgentFields adds the agent tool config to a widget payload.
// Shared by the create and update payload builders (single code path).
func applyWidgetAgentFields(out map[string]interface{}, cfg widgetConfig) {
	if cfg.AgentEnabled != nil {
		out["agent_enabled"] = *cfg.AgentEnabled
	}
	if cfg.AgentMCPServer != "" {
		out["agent_mcp_server"] = cfg.AgentMCPServer
	}
	if len(cfg.AgentMCPTools) > 0 {
		out["agent_mcp_tools"] = cfg.AgentMCPTools
	}
}

// applyWidgetMediaFields adds the file-upload + audio-recording toggles.
func applyWidgetMediaFields(out map[string]interface{}, cfg widgetConfig) {
	if cfg.FileUploadEnabled != nil {
		out["file_upload_enabled"] = *cfg.FileUploadEnabled
	}
	if len(cfg.AllowedMimeTypes) > 0 {
		// API expects comma-separated string (DB stores it that way too).
		out["allowed_mime_types"] = strings.Join(cfg.AllowedMimeTypes, ",")
	}
	if cfg.MaxFileSizeMB > 0 {
		out["max_file_size_mb"] = cfg.MaxFileSizeMB
	}
	if cfg.AudioRecordingEnabled != nil {
		out["audio_recording_enabled"] = *cfg.AudioRecordingEnabled
	}
}

// widgetUpdatePayload mirrors UpdateRequest.
func widgetUpdatePayload(cfg widgetConfig) map[string]interface{} {
	out := map[string]interface{}{
		"name":        cfg.Name,
		"description": cfg.Description,
	}
	if cfg.Slug != "" {
		out["slug"] = cfg.Slug
	}
	if cfg.Enabled != nil {
		out["enabled"] = *cfg.Enabled
	}
	if cfg.SystemPrompt != "" {
		out["system_prompt"] = cfg.SystemPrompt
	}
	if cfg.FirstMessage != "" {
		out["first_message"] = cfg.FirstMessage
	}
	if cfg.AIProvider != "" {
		out["ai_provider"] = cfg.AIProvider
	}
	if cfg.AIModel != "" {
		out["ai_model"] = cfg.AIModel
	}
	if cfg.MaxTokens > 0 {
		out["max_tokens"] = cfg.MaxTokens
	}
	if cfg.Temperature > 0 {
		out["temperature"] = cfg.Temperature
	}
	if cfg.RateLimitPerMin > 0 {
		out["rate_limit_per_min"] = cfg.RateLimitPerMin
	}
	if cfg.AllowAnonymous != nil {
		out["allow_anonymous"] = *cfg.AllowAnonymous
	}
	if cfg.AllowedOrigins != "" {
		out["allowed_origins"] = cfg.AllowedOrigins
	}
	if cfg.DefaultLanguage != "" {
		out["default_language"] = cfg.DefaultLanguage
	}
	if len(cfg.AllowedLanguages) > 0 {
		out["allowed_languages"] = cfg.AllowedLanguages
	}
	if cfg.GoalsDescription != "" {
		out["goals_description"] = cfg.GoalsDescription
	}
	if cfg.IntakeQuestionsJSON != "" {
		out["intake_questions_json"] = cfg.IntakeQuestionsJSON
	}
	if cfg.LLMValidationEnabled != nil {
		out["llm_validation_enabled"] = *cfg.LLMValidationEnabled
	}
	if cfg.PageAwarenessEnabled != nil {
		out["page_awareness_enabled"] = *cfg.PageAwarenessEnabled
	}
	if cfg.KnowledgeSourcesEnabled != nil {
		out["knowledge_sources_enabled"] = *cfg.KnowledgeSourcesEnabled
	}
	if cfg.PassthroughSystemMessages != nil {
		out["passthrough_system_messages"] = *cfg.PassthroughSystemMessages
	}
	applyWidgetAgentFields(out, cfg)
	applyWidgetMediaFields(out, cfg)
	return out
}
