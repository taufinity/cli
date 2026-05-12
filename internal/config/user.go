package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// UserConfig holds user-level configuration stored in ~/.config/taufinity/config.yaml.
type UserConfig struct {
	Site   string `yaml:"site,omitempty"`
	APIURL string `yaml:"api_url,omitempty"`
	Org    string `yaml:"org,omitempty"`

	// UpdateCheck controls the background staleness check. Empty = enabled,
	// "false" = disabled. Kept as a string to stay consistent with the other
	// string-valued config keys handled by Set/Get/List.
	UpdateCheck string `yaml:"update_check,omitempty"`
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
	case "update_check":
		if value != "" && value != "true" && value != "false" {
			return fmt.Errorf("update_check must be true, false, or empty (got %q)", value)
		}
		cfg.UpdateCheck = value
	default:
		return fmt.Errorf("unknown config key: %s (valid: site, api_url, update_check)", key)
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
	case "update_check":
		return cfg.UpdateCheck, nil
	default:
		return "", fmt.Errorf("unknown config key: %s (valid: site, api_url, update_check)", key)
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
		"update_check": cfg.UpdateCheck,
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
	case "update_check":
		cfg.UpdateCheck = ""
	default:
		return fmt.Errorf("unknown config key: %s (valid: site, api_url, update_check)", key)
	}

	return cfg.Save()
}
