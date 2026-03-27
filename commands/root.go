package commands

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/taufinity/cli/internal/config"
)

// DefaultAPIURL is the default Taufinity API endpoint.
const DefaultAPIURL = "https://studio.taufinity.io"

var (
	// Global flags
	flagSite   string
	flagAPIURL string
	flagOrg    string
	flagFormat string
	flagQuiet  bool
	flagDryRun bool
	flagDebug  bool

	// Resolved config (loaded on init)
	cfg *config.UserConfig
)

var rootCmd = &cobra.Command{
	Use:   "taufinity",
	Short: "Taufinity CLI - template development and preview",
	Long: `Taufinity CLI helps template developers preview renders locally.

Use 'taufinity auth login' to authenticate, then 'taufinity template preview'
to upload and preview your templates.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Load user config
		var err error
		cfg, err = config.Load()
		if err != nil {
			// Config doesn't exist yet, use defaults
			cfg = &config.UserConfig{}
		}

		// Apply flag overrides (flags > env > config > default)
		resolveAPIURL()
		resolveSite()

		return nil
	},
}

func init() {
	// Global flags available to all commands
	rootCmd.PersistentFlags().StringVar(&flagSite, "site", "", "Override site ID")
	rootCmd.PersistentFlags().StringVar(&flagAPIURL, "api-url", "", "Override API URL")
	rootCmd.PersistentFlags().StringVar(&flagOrg, "org", "", "Override organization ID (for playbook commands)")
	rootCmd.PersistentFlags().StringVar(&flagFormat, "format", "table", "Output format: table, json, yaml")
	rootCmd.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "Minimal output, no prompts")
	rootCmd.PersistentFlags().BoolVar(&flagDryRun, "dry-run", false, "Print API calls without executing")
	rootCmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "Print all API requests/responses for debugging")
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// resolveAPIURL determines the API URL from flags, env, config, or default.
func resolveAPIURL() {
	if flagAPIURL != "" {
		return // Flag takes precedence
	}
	if env := os.Getenv("TAUFINITY_API_URL"); env != "" {
		flagAPIURL = env
		return
	}
	if cfg != nil && cfg.APIURL != "" {
		flagAPIURL = cfg.APIURL
		return
	}
	flagAPIURL = DefaultAPIURL
}

// siteSource tracks where flagSite was set from, for priority resolution.
// Priority: flag > env > project config (taufinity.yaml) > user config
var siteSource string

// resolveSite determines the site from flags, env, config, or project config.
func resolveSite() {
	if flagSite != "" {
		siteSource = "flag"
		return // Flag takes precedence
	}
	if env := os.Getenv("TAUFINITY_SITE"); env != "" {
		flagSite = env
		siteSource = "env"
		return
	}
	if cfg != nil && cfg.Site != "" {
		flagSite = cfg.Site
		siteSource = "user-config"
		return
	}
	// Site can also come from project config (taufinity.yaml)
	// This is loaded later in commands that need it via SetSite
}

// GetAPIURL returns the resolved API URL.
func GetAPIURL() string {
	return flagAPIURL
}

// GetSite returns the resolved site ID.
func GetSite() string {
	return flagSite
}

// SetSite sets the site from project config (taufinity.yaml).
// Project config overrides user config but not flag or env.
func SetSite(site string) {
	if flagSite == "" || siteSource == "user-config" {
		flagSite = site
		siteSource = "project-config"
	}
}

// GetOrg returns the resolved organization ID.
func GetOrg() string {
	if flagOrg != "" {
		return flagOrg
	}
	if env := os.Getenv("TAUFINITY_ORG"); env != "" {
		return env
	}
	if cfg != nil && cfg.Org != "" {
		return cfg.Org
	}
	return ""
}

// IsQuiet returns whether quiet mode is enabled.
func IsQuiet() bool {
	return flagQuiet || os.Getenv("TAUFINITY_QUIET") == "1"
}

// IsDryRun returns whether dry-run mode is enabled.
func IsDryRun() bool {
	return flagDryRun || os.Getenv("TAUFINITY_DRY_RUN") == "1"
}

// IsDebug returns whether debug mode is enabled.
func IsDebug() bool {
	return flagDebug || os.Getenv("TAUFINITY_DEBUG") == "1"
}

// GetFormat returns the output format.
func GetFormat() string {
	return flagFormat
}

// Print prints a message unless quiet mode is enabled.
func Print(format string, args ...any) {
	if !IsQuiet() {
		fmt.Printf(format, args...)
	}
}

// PrintLn prints a line unless quiet mode is enabled.
func PrintLn(msg string) {
	if !IsQuiet() {
		fmt.Println(msg)
	}
}

// printJSON outputs data as JSON.
func printJSON(data any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

// printYAML outputs data as YAML.
func printYAML(data any) error {
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	return enc.Encode(data)
}
