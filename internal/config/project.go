package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectConfigFile is the filename for per-project configuration.
const ProjectConfigFile = "taufinity.yaml"

// ProjectConfig holds per-project configuration from taufinity.yaml.
type ProjectConfig struct {
	Site        string   `yaml:"site,omitempty"`
	Template    string   `yaml:"template,omitempty"`
	PreviewData string   `yaml:"preview_data,omitempty"`
	Ignore      []string `yaml:"ignore,omitempty"`
	Warnings    []string `yaml:"-"` // Populated during loading (e.g. unknown keys)
}

// validProjectKeys lists all recognized keys in taufinity.yaml.
var validProjectKeys = map[string]bool{
	"site":         true,
	"template":     true,
	"preview_data": true,
	"ignore":       true,
}

// LoadProject reads the project config from the given directory.
func LoadProject(dir string) (*ProjectConfig, error) {
	configFile := filepath.Join(dir, ProjectConfigFile)

	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectConfig{}, nil
		}
		return nil, fmt.Errorf("read project config: %w", err)
	}

	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse project config: %w", err)
	}

	// Check for unknown keys
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err == nil {
		for key := range raw {
			if !validProjectKeys[key] {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("unknown key %q in %s (valid keys: site, template, preview_data, ignore)", key, ProjectConfigFile))
			}
		}
	}

	return &cfg, nil
}

// FindProjectRoot walks up the directory tree to find taufinity.yaml.
func FindProjectRoot(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}

	for {
		configPath := filepath.Join(dir, ProjectConfigFile)
		if _, err := os.Stat(configPath); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached root
			return "", fmt.Errorf("%s not found in directory tree", ProjectConfigFile)
		}
		dir = parent
	}
}

// LoadProjectFromCwd finds and loads project config from current directory or parents.
func LoadProjectFromCwd() (*ProjectConfig, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("get cwd: %w", err)
	}

	root, err := FindProjectRoot(cwd)
	if err != nil {
		// No project config found - return empty config
		return &ProjectConfig{}, cwd, nil
	}

	cfg, err := LoadProject(root)
	if err != nil {
		return nil, "", err
	}

	return cfg, root, nil
}
