package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/taufinity/cli/internal/buildinfo"
	"github.com/taufinity/cli/internal/config"
	"github.com/taufinity/cli/internal/updatecheck"
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
	// Wire the staleness check around the cobra invocation. We always run the
	// command itself even if the check stalls or errors — the check is a side
	// effect, never a gate.
	checker := startUpdateCheck()
	err := rootCmd.Execute()
	maybeWarnAtExit(checker)
	return err
}

// updateChecker captures the inputs needed to print the staleness warning at
// exit. We resolve the running command's annotations and the user-config flag
// up front so the background goroutine can run independently of cobra state.
type updateChecker struct {
	runner          *updatecheck.Runner
	suppressByCmd   bool
	configDisabled  bool
}

// startUpdateCheck kicks off the staleness check in a background goroutine if
// the command and environment allow it. Returns a checker the caller passes to
// maybeWarnAtExit. Always returns a non-nil pointer to keep the call sites
// trivial.
func startUpdateCheck() *updateChecker {
	c := &updateChecker{}

	// Quick reject: env opt-out — skip both the goroutine and the warning.
	if os.Getenv(updatecheck.EnvDisable) == "1" {
		return c
	}

	// Resolve the command being run (without executing it). cobra exposes a
	// dry-run Find; if it errors out (e.g. unknown command, --help), we just
	// skip the check rather than guessing.
	args := os.Args[1:]
	cmd, _, err := rootCmd.Find(args)
	if err != nil || cmd == nil {
		return c
	}
	if hasSuppressAnnotation(cmd) {
		c.suppressByCmd = true
		return c
	}

	// Config opt-out (read here once, used both to skip the goroutine and to
	// pass into MaybeWarn at exit for completeness).
	if cfg != nil && cfg.UpdateCheck == "false" {
		c.configDisabled = true
		return c
	}

	// Cache fresh? Skip the network entirely.
	cache := updatecheck.LoadCache()
	if cache.IsFresh(time.Now(), updatecheck.DefaultCacheMaxAge) {
		return c
	}

	c.runner = &updatecheck.Runner{
		Debug: debugWriter(),
	}
	// Use Background, not cmd.Context() — cobra contexts get cancelled the
	// moment a RunE returns, and the goroutine should be allowed to complete
	// its bounded wait independently.
	c.runner.Start(context.Background())
	return c
}

// maybeWarnAtExit waits briefly for the background check (if one was started)
// and prints the warning to stderr if the running binary is behind.
func maybeWarnAtExit(c *updateChecker) {
	if c == nil || c.suppressByCmd {
		return
	}
	if c.runner != nil {
		// 100ms is enough for a healthy local network; longer would noticeably
		// delay short-lived commands like `auth status`. If the goroutine isn't
		// done, we just don't write the cache this run — next run picks it up.
		c.runner.Wait(100 * time.Millisecond)
	}
	cache := updatecheck.LoadCache()
	info := buildinfo.FromBuildtime(Version, GitCommit, BuildTime)
	updatecheck.MaybeWarn(os.Stderr, info, cache, updatecheck.Options{
		Quiet:           IsQuiet(),
		ConfigDisabled:  c.configDisabled,
		CommandSuppress: c.suppressByCmd,
	})
}

// hasSuppressAnnotation walks the cobra command tree from the resolved command
// up to root and returns true if any node carries the suppress annotation. The
// tree walk lets us suppress whole subtrees (e.g. all `mcp` subcommands) by
// annotating an ancestor, though today we annotate per-command.
func hasSuppressAnnotation(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[updatecheck.AnnotationSuppress] == "true" {
			return true
		}
	}
	return false
}

// debugWriter returns os.Stderr when --debug is set, otherwise io.Discard.
// (We can't call IsDebug() here without parsing flags first; this is invoked
// before cobra has resolved global flags. Fall back to env-only check.)
func debugWriter() io.Writer {
	if os.Getenv("TAUFINITY_DEBUG") == "1" {
		return os.Stderr
	}
	return io.Discard
}

// resolveAPIURL determines the API URL from flags, env, config, or default.
func resolveAPIURL() {
	if flagAPIURL != "" {
		warnIfLocalhost()
		return // Flag takes precedence
	}
	if env := os.Getenv("TAUFINITY_API_URL"); env != "" {
		flagAPIURL = env
		warnIfLocalhost()
		return
	}
	if cfg != nil && cfg.APIURL != "" {
		flagAPIURL = cfg.APIURL
		warnIfLocalhost()
		return
	}
	flagAPIURL = DefaultAPIURL
}

// warnIfLocalhost prints a warning to stderr when the API URL points to localhost.
func warnIfLocalhost() {
	if IsQuiet() {
		return
	}
	if strings.Contains(flagAPIURL, "localhost") || strings.Contains(flagAPIURL, "127.0.0.1") {
		fmt.Fprintf(os.Stderr, "Warning: API URL points to localhost (%s) — you may be talking to a dev server instead of production.\n", flagAPIURL)
	}
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
