// provision_tracker.go — per-site analytics tracker settings.
//
// One file per site: sites/<site>/tracker.yaml → PUT /api/sites/{id}/settings/tracker.
// Missing file is a clean no-op, matching the other per-site settings sections.
//
//	enabled: true
//	write_key: <source write key from your analytics pipeline>
//	host: https://events.example.com
//	consent_mode: opt_in
//	forwarder_destinations:
//	  - google_ads
//	event_conversion_map:
//	  signup: CompleteRegistration
//
// The write key identifies the site's source in the event pipeline. If it does
// not match a real source, the collector rejects every event at ingest, so the
// key is required whenever tracking is enabled — an empty key is a
// misconfiguration that would otherwise only surface as silently missing data.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// trackerConfig is the YAML shape of tracker.yaml. The JSON tags match the
// settings section the API stores, so it round-trips without transformation.
type trackerConfig struct {
	Enabled               bool              `yaml:"enabled"                json:"enabled"`
	WriteKey              string            `yaml:"write_key"              json:"write_key,omitempty"`
	Host                  string            `yaml:"host"                   json:"host,omitempty"`
	ConsentMode           string            `yaml:"consent_mode"           json:"consent_mode,omitempty"`
	ForwarderDestinations []string          `yaml:"forwarder_destinations" json:"forwarder_destinations,omitempty"`
	EventConversionMap    map[string]string `yaml:"event_conversion_map"   json:"event_conversion_map,omitempty"`
}

// validateTrackerConfig rejects configurations that would deploy cleanly but
// drop every event.
func validateTrackerConfig(cfg trackerConfig) error {
	if cfg.Enabled && cfg.WriteKey == "" {
		return fmt.Errorf("write_key is required when enabled: true")
	}
	return nil
}

// provisionTracker applies sites/<site>/tracker.yaml for one site.
func provisionTracker(c *provisionClient, siteID uint, siteDir string) error {
	path := filepath.Join(siteDir, "tracker.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read tracker.yaml: %w", err)
	}

	var cfg trackerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse tracker.yaml: %w", err)
	}
	if err := validateTrackerConfig(cfg); err != nil {
		return fmt.Errorf("tracker.yaml: %w", err)
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal tracker settings: %w", err)
	}

	body, status, err := c.put(fmt.Sprintf("/sites/%d/settings/tracker", siteID), payload)
	if err != nil || status >= 300 {
		return provisionAPIErr(fmt.Sprintf("update tracker settings for site %d", siteID), status, body, err)
	}

	fmt.Printf("provision: site %d tracker settings updated (enabled=%v)\n", siteID, cfg.Enabled)
	return nil
}
