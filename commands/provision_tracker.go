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
// not resolve to a real source, the collector rejects (401s) every event at
// ingest. Nothing about that failure is loud: the site deploys, the tracker
// loads, provision reports success, and every event is dropped on the floor
// until somebody notices the dashboards are empty. That is precisely the class
// of silent, deploy-clean failure this tool exists to catch, so the key is
// checked here — before it can be written — whenever we have something to
// check it against.
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

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

// writeKeyRe pulls the source write keys out of an analytics workspace config.
//
// That file is an infrastructure template, not plain JSON — it carries
// interpolation placeholders, so a JSON parser rejects it. The write keys are
// literal strings regardless, so we match them literally. Parsing the template
// as JSON is not an option; matching the one field we need is.
var writeKeyRe = regexp.MustCompile(`"writeKey"\s*:\s*"([^"]+)"`)

// writeKeysInWorkspaceConfig returns every source write key declared in the
// given workspace-config text.
func writeKeysInWorkspaceConfig(text string) []string {
	matches := writeKeyRe.FindAllStringSubmatch(text, -1)
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		keys = append(keys, m[1])
	}
	return keys
}

// validateWriteKeyInWorkspaceConfig is the pure check: the key must be declared
// as a source in the workspace config. An unknown key is a hard failure, not a
// warning — shipping it means the collector 401s every event, and we know that
// for certain here, so there is nothing to be tentative about.
func validateWriteKeyInWorkspaceConfig(writeKey, workspaceConfig string) error {
	if writeKey == "" {
		return errors.New("write_key is empty")
	}
	keys := writeKeysInWorkspaceConfig(workspaceConfig)
	for _, k := range keys {
		if k == writeKey {
			return nil
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("workspace config declares no sources at all — is it the right file?")
	}
	return fmt.Errorf(
		"write_key %q is not a source in the workspace config (%d source(s) declared). "+
			"Add the source to the analytics workspace config and apply it BEFORE pushing this "+
			"site config, or the collector will reject every event",
		writeKey, len(keys))
}

// validateTrackerConfig rejects configurations that would deploy cleanly but
// drop every event.
func validateTrackerConfig(cfg trackerConfig) error {
	if cfg.Enabled && cfg.WriteKey == "" {
		return fmt.Errorf("write_key is required when enabled: true")
	}
	return nil
}

// checkTrackerWriteKey validates cfg.WriteKey against the workspace config the
// operator supplied via --workspace-config (or TAUFINITY_WORKSPACE_CONFIG).
//
// The workspace config lives with the analytics infrastructure, which is not
// necessarily in the same repo as the site specs, so provision cannot go and
// find it — it has to be handed one. When it is not, we do NOT quietly skip:
// a safety check that disappears without saying anything is how the bug it was
// meant to catch survives. We warn, loudly, and say what the consequence is.
func checkTrackerWriteKey(c *provisionClient, cfg trackerConfig) error {
	if cfg.WriteKey == "" {
		// Nothing to check. validateTrackerConfig has already rejected the
		// dangerous version of this (enabled with no key).
		return nil
	}

	if c.workspaceConfigPath == "" {
		c.Warn("tracker write_key could not be validated: no workspace config supplied " +
			"(pass --workspace-config <path>, or set TAUFINITY_WORKSPACE_CONFIG). " +
			"If this key is not a declared source, the tracker will deploy successfully " +
			"and then silently drop every event.")
		return nil
	}

	data, err := os.ReadFile(c.workspaceConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		// An explicitly supplied path that does not exist is an operator error,
		// not a reason to fall back to the unvalidated path.
		return fmt.Errorf("workspace config %s does not exist", c.workspaceConfigPath)
	}
	if err != nil {
		return fmt.Errorf("read workspace config %s: %w", c.workspaceConfigPath, err)
	}
	if err := validateWriteKeyInWorkspaceConfig(cfg.WriteKey, string(data)); err != nil {
		return err
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
	if err := checkTrackerWriteKey(c, cfg); err != nil {
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
