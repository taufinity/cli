package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// OrgIDPattern matches the org identifiers the CLI accepts as a default org
// or embeds verbatim into MCP stdio-bridge args: numeric IDs in practice,
// slugs for future-proofing. The value travels as an HTTP header and as a
// client-config argument, so anything else (whitespace, quotes, a leading
// dash that reads as a flag) is refused rather than stored.
var OrgIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

const validKeys = "site, api_url, org, update_check, auto_update"

// UserConfig holds user-level configuration stored in ~/.config/taufinity/config.yaml.
type UserConfig struct {
	Site   string `yaml:"site,omitempty"`
	APIURL string `yaml:"api_url,omitempty"`
	Org    string `yaml:"org,omitempty"`

	// UpdateCheck controls the background staleness check. Empty = enabled,
	// "false" = disabled. Kept as a string to stay consistent with the other
	// string-valued config keys handled by Set/Get/List.
	UpdateCheck string `yaml:"update_check,omitempty"`

	// AutoUpdate, when "true", automatically runs `taufinity update` at exit
	// whenever a newer version is detected. The update uses the same backup +
	// smoke-test + auto-rollback logic as the manual `taufinity update` command,
	// so a bad build is reverted automatically and users are never locked out.
	// Opt in: taufinity config set auto_update true
	AutoUpdate string `yaml:"auto_update,omitempty"`
}

// Dir returns the path to the taufinity config directory.
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".config", "taufinity")
}

// configPath returns the path to the config file.
func configPath() string {
	return filepath.Join(Dir(), "config.yaml")
}

// Load reads the user config from disk.
func Load() (*UserConfig, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &UserConfig{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg UserConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return &cfg, nil
}

// Save writes the user config to disk.
func (c *UserConfig) Save() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(configPath(), data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// Set sets a config property and saves.
func Set(key, value string) error {
	cfg, err := Load()
	if err != nil {
		cfg = &UserConfig{}
	}

	switch key {
	case "site":
		cfg.Site = value
	case "api_url":
		cfg.APIURL = value
	case "org":
		if value != "" && !OrgIDPattern.MatchString(value) {
			return fmt.Errorf("org must be a numeric organization ID or slug (got %q); see: taufinity org list", value)
		}
		cfg.Org = value
	case "update_check":
		if value != "" && value != "true" && value != "false" {
			return fmt.Errorf("update_check must be true, false, or empty (got %q)", value)
		}
		cfg.UpdateCheck = value
	case "auto_update":
		if value != "" && value != "true" && value != "false" {
			return fmt.Errorf("auto_update must be true, false, or empty (got %q)", value)
		}
		cfg.AutoUpdate = value
	default:
		return fmt.Errorf("unknown config key: %s (valid: %s)", key, validKeys)
	}

	return cfg.Save()
}

// Get returns a config property value.
func Get(key string) (string, error) {
	cfg, err := Load()
	if err != nil {
		return "", err
	}

	switch key {
	case "site":
		return cfg.Site, nil
	case "api_url":
		return cfg.APIURL, nil
	case "org":
		return cfg.Org, nil
	case "update_check":
		return cfg.UpdateCheck, nil
	case "auto_update":
		return cfg.AutoUpdate, nil
	default:
		return "", fmt.Errorf("unknown config key: %s (valid: %s)", key, validKeys)
	}
}

// List returns all config properties as a map.
func List() (map[string]string, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"site":         cfg.Site,
		"api_url":      cfg.APIURL,
		"org":          cfg.Org,
		"update_check": cfg.UpdateCheck,
		"auto_update":  cfg.AutoUpdate,
	}, nil
}

// Reset removes the config file, resetting all settings to defaults.
func Reset() error {
	path := configPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove config: %w", err)
	}
	return nil
}

// Unset removes a specific config property.
func Unset(key string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}

	switch key {
	case "site":
		cfg.Site = ""
	case "api_url":
		cfg.APIURL = ""
	case "org":
		cfg.Org = ""
	case "update_check":
		cfg.UpdateCheck = ""
	case "auto_update":
		cfg.AutoUpdate = ""
	default:
		return fmt.Errorf("unknown config key: %s (valid: %s)", key, validKeys)
	}

	return cfg.Save()
}
