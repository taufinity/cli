// provision_site_settings.go — provision support for three per-site
// settings sections that steer content generation:
//
//   - general-settings.yaml  → PUT /api/sites/{id}/settings/general
//     description, enabled, show_excerpt — the description feeds
//     {{.Description}} in topic_generation.txt.
//   - content-settings.yaml  → PUT /api/sites/{id}/settings/content
//     category, format, tone, length, keywords, languages — feed
//     {{.Category}}, {{.Format}}, {{.Tone}} in the prompts.
//   - metadata-settings.yaml → PUT /api/sites/{id}/settings/metadata
//     free-form keys; conventionally target_audience lives here and
//     feeds {{.TargetAudience}}. The metadata handler accepts any keys,
//     so this struct is intentionally a map.
//
// Same shape as provisionAISettings (provision_sites.go):
// read YAML → marshal JSON → PUT → log. Missing file is a clean no-op,
// matching ai-settings.yaml. Idempotent — re-running with no content
// change is a no-op write; a field change is an in-place update.
//
// Versioning: each PUT is recorded as one entity_versions row on
// site_config via the X-Change-Source: provision header (see
// provision_client.go writeWithHeaders).
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// generalSettingsConfig mirrors the allowlist in the API handler
// (api/handlers/sites_settings.go UpdateGeneralSettings). Fields outside
// this allowlist are silently ignored by the API; we omit them here so a
// typo surfaces as "field not pushed" rather than "field accepted but
// dropped."
type generalSettingsConfig struct {
	Description string `yaml:"description"   json:"description,omitempty"`
	Enabled     *bool  `yaml:"enabled"       json:"enabled,omitempty"`
	ShowExcerpt *bool  `yaml:"show_excerpt"  json:"show_excerpt,omitempty"`
}

// contentSettingsConfig mirrors the allowlist in UpdateContentSettings.
// Topic discovery reads Category and Format off this section
// (internal/content/topics.go buildTopicGenerationPrompt). Tone, length,
// languages are here for completeness and forward-compat; passing them
// is harmless when the site doesn't need them.
type contentSettingsConfig struct {
	Category  string   `yaml:"category"   json:"category,omitempty"`
	Format    string   `yaml:"format"     json:"format,omitempty"`
	Tone      string   `yaml:"tone"       json:"tone,omitempty"`
	Length    string   `yaml:"length"     json:"length,omitempty"`
	Languages []string `yaml:"languages"  json:"languages,omitempty"`
	Keywords  []string `yaml:"keywords"   json:"keywords,omitempty"`
}

// metadataSettingsConfig is intentionally a free-form map. The API handler
// (UpdateMetadataSettings) loops over every key in the request body and
// writes it through, so capturing a fixed schema here would be too
// restrictive. target_audience is the standard convention (read by
// buildTopicGenerationPrompt as siteConfig.Metadata["target_audience"]).
type metadataSettingsConfig map[string]any

func provisionGeneralSettings(c *provisionClient, siteID uint, siteDir string) error {
	return provisionSettingsYAML(c, siteID, siteDir, "general-settings.yaml", "general",
		func(data []byte) ([]byte, error) {
			var cfg generalSettingsConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			return json.Marshal(cfg)
		})
}

func provisionContentSettings(c *provisionClient, siteID uint, siteDir string) error {
	return provisionSettingsYAML(c, siteID, siteDir, "content-settings.yaml", "content",
		func(data []byte) ([]byte, error) {
			var cfg contentSettingsConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			return json.Marshal(cfg)
		})
}

func provisionMetadataSettings(c *provisionClient, siteID uint, siteDir string) error {
	return provisionSettingsYAML(c, siteID, siteDir, "metadata-settings.yaml", "metadata",
		func(data []byte) ([]byte, error) {
			var cfg metadataSettingsConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			return json.Marshal(cfg)
		})
}

// provisionSettingsYAML is the shared body of the three per-section
// provisioners. Reads <siteDir>/<filename>, runs `parse` to validate and
// re-emit JSON, PUTs to /sites/{id}/settings/<section>. Missing file is a
// no-op (every settings file is opt-in).
func provisionSettingsYAML(
	c *provisionClient,
	siteID uint,
	siteDir, filename, section string,
	parse func([]byte) ([]byte, error),
) error {
	path := filepath.Join(siteDir, filename)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}

	payload, err := parse(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", filename, err)
	}

	apiPath := fmt.Sprintf("/sites/%d/settings/%s", siteID, section)
	respBody, status, err := c.put(apiPath, payload)
	if err != nil || status >= 300 {
		return provisionAPIErr(fmt.Sprintf("update %s settings for site %d", section, siteID), status, respBody, err)
	}

	fmt.Printf("provision: site %d %s settings updated\n", siteID, section)
	return nil
}
