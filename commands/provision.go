package commands

import (
	"bytes"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Provision command flags
var (
	provisionDir              string
	provisionOrgSlug          string
	provisionAPIKey           string
	provisionStrict           bool
	provisionPruneIdentifiers bool
	provisionNoInviteEmail    bool
	provisionDraft            bool
	provisionPreviewDataset   string
	provisionAllowDrift       bool
	provisionWorkspaceConfig  string
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Apply customer Studio configuration from YAML/JSON specs",
	Long: `Provision pushes all Studio config resources (providers, portal,
dashboards, pipelines, playbooks, widgets, and more) from a local studio/
directory to a Studio instance.

This is the CLI replacement for 'go run ./cmd/provision' in ai-site-gen.
All existing studio/ directory layouts and YAML/JSON schemas are unchanged.

Authentication requires a bootstrap admin API key. Supply it via --api-key
or the TAUFINITY_ADMIN_TOKEN environment variable.

Examples:
  # Dry-run (preview changes)
  taufinity provision diff --dir ../felix/studio --org felix-works

  # Apply to localhost
  taufinity --api-url http://localhost:8090 provision apply \
      --dir ../felix/studio --org felix-works

  # Apply to prod
  taufinity provision apply --dir ../felix/studio --org felix-works \
      --api-key "$TAUFINITY_ADMIN_TOKEN"

  # Pull dashboards from prod back to local specs
  taufinity provision pull dashboards --dir ../felix/studio --org felix-works`,
}

var provisionApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply all Studio config resources from the spec directory",
	RunE:  runProvisionApply,
}

var provisionDiffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Dry-run: show what apply would change without mutating anything",
	RunE: func(cmd *cobra.Command, args []string) error {
		flagDryRun = true
		return runProvisionApply(cmd, args)
	},
}

var provisionPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull Studio config back from a running instance to local spec files",
}

func init() {
	rootCmd.AddCommand(provisionCmd)
	provisionCmd.AddCommand(provisionApplyCmd)
	provisionCmd.AddCommand(provisionDiffCmd)
	provisionCmd.AddCommand(provisionPullCmd)

	for _, cmd := range []*cobra.Command{provisionApplyCmd, provisionDiffCmd} {
		cmd.Flags().StringVar(&provisionDir, "dir", "", "Customer config directory (required)")
		cmd.Flags().StringVar(&provisionOrgSlug, "org", "", "Organization slug (required)")
		cmd.Flags().StringVar(&provisionAPIKey, "api-key", "", "Bootstrap admin API key (overrides TAUFINITY_ADMIN_TOKEN env)")
		cmd.Flags().BoolVar(&provisionStrict, "strict", false, "Exit 2 on dashboard drift, exit 3 on warnings")
		cmd.Flags().BoolVar(&provisionPruneIdentifiers, "prune-identifiers", false, "Remove client_group identifiers not in YAML (data-loss risk — opt-in)")
		cmd.Flags().BoolVar(&provisionNoInviteEmail, "no-invite-email", false, "Create invitations without sending email (for testing)")
		cmd.Flags().BoolVar(&provisionDraft, "draft", false, "Push dashboards as admin-only preview versions")
		cmd.Flags().StringVar(&provisionPreviewDataset, "preview-dataset", "", "BQ dataset override for preview mode (use with --draft)")
		cmd.Flags().StringVar(&provisionWorkspaceConfig, "workspace-config", "", "Path to the analytics workspace config declaring valid source write keys. Used to validate each site's tracker write_key before it is pushed. Without it the key cannot be checked, and an unknown key means the tracker deploys fine and then silently drops every event. Env: TAUFINITY_WORKSPACE_CONFIG")
		cmd.Flags().BoolVar(&provisionAllowDrift, "allow-drift", false, "Apply playbooks even when the diff is classified HIGH drift (deleted steps, AI model/provider change, error-policy change, schedule/enabled flip, wholesale step rewrite). Default off: a HIGH-drift apply aborts so a stale local file can't silently revert live config")
		_ = cmd.MarkFlagRequired("dir")
		_ = cmd.MarkFlagRequired("org")
	}

	for _, cmd := range []*cobra.Command{provisionPullCmd} {
		cmd.PersistentFlags().StringVar(&provisionDir, "dir", "", "Customer config directory (required)")
		cmd.PersistentFlags().StringVar(&provisionOrgSlug, "org", "", "Organization slug (required)")
		cmd.PersistentFlags().StringVar(&provisionAPIKey, "api-key", "", "Bootstrap admin API key")
		_ = cmd.MarkPersistentFlagRequired("dir")
		_ = cmd.MarkPersistentFlagRequired("org")
	}
}

// resolveProvisionAPIKey returns the admin API key from flag or env.
func resolveProvisionAPIKey() (string, error) {
	if provisionAPIKey != "" {
		return provisionAPIKey, nil
	}
	if v := os.Getenv("TAUFINITY_ADMIN_TOKEN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("SITEGEN_API_KEY"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("admin API key required: pass --api-key or set TAUFINITY_ADMIN_TOKEN")
}

// resolveWorkspaceConfigPath returns the analytics workspace-config path from
// the flag, falling back to the environment. Empty means "not supplied", which
// leaves tracker write keys unvalidated — checkTrackerWriteKey warns about it
// rather than passing over it in silence.
func resolveWorkspaceConfigPath() string {
	if provisionWorkspaceConfig != "" {
		return provisionWorkspaceConfig
	}
	return os.Getenv("TAUFINITY_WORKSPACE_CONFIG")
}

// fileExists returns true if path exists on disk (file or dir).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mustReadYAML reads a YAML file into v with KnownFields(true) so unknown/misspelled
// keys are rejected at parse time (matches original provision tool behaviour).
func mustReadYAML(path string, v interface{}) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision: read %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := yamlUnmarshalStrict(data, v); err != nil {
		fmt.Fprintf(os.Stderr, "provision: parse %s: %v\n", path, err)
		os.Exit(1)
	}
}

// yamlUnmarshalStrict decodes YAML with KnownFields(true) so unknown/misspelled
// keys are rejected at parse time. Matches original provision tool (main.go:501-508).
func yamlUnmarshalStrict(data []byte, v interface{}) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// runProvisionApply is the main orchestration loop.
// Order follows the dependency graph: org members → providers → portal →
// kpi → client groups → nav → dashboards → sites → credentials →
// image taxonomy → playbooks → test suites → router rules → widgets →
// knowledge → images manifest.
func runProvisionApply(cmd *cobra.Command, args []string) error {
	key, err := resolveProvisionAPIKey()
	if err != nil {
		return err
	}

	apiURL := GetAPIURL()
	dryRun := IsDryRun()

	c := newProvisionClient(apiURL, key, dryRun)
	c.noInviteEmail = provisionNoInviteEmail
	c.workspaceConfigPath = resolveWorkspaceConfigPath()

	orgID, err := resolveProvisionOrgID(c, provisionOrgSlug)
	if err != nil {
		return fmt.Errorf("resolve org %q: %w", provisionOrgSlug, err)
	}
	fmt.Printf("provision: org %q = id %d\n", provisionOrgSlug, orgID)

	dir := provisionDir

	// 1. Org members
	if err := applyOrgMembers(c, dir, orgID); err != nil {
		return err
	}
	// 2. Providers
	providerID, err := applyProviders(c, dir, orgID)
	if err != nil {
		return err
	}
	// 3. Portal
	if err := applyPortal(c, dir, orgID); err != nil {
		return err
	}
	// 4. Feature flag overrides — applied before KPI so subsequent steps can rely on final flag state
	if err := applyFeatureFlags(c, dir, orgID); err != nil {
		return err
	}
	// 5. KPI
	if err := applyKPI(c, dir, orgID); err != nil {
		return err
	}
	// 6. Client groups
	if err := applyClientGroups(c, dir, orgID, provisionPruneIdentifiers); err != nil {
		return err
	}
	// 7. Nav
	if err := applyNav(c, dir, orgID); err != nil {
		return err
	}
	// 8. Dashboards
	driftCount, err := applyDashboards(c, dir, orgID, providerID, provisionDraft, provisionPreviewDataset)
	if err != nil {
		return err
	}
	// 9. Sites (pipeline, secure-render, AI settings)
	if err := applySites(c, dir, orgID); err != nil {
		return err
	}
	// 10. Credentials
	if err := applyCredentials(c, dir, orgID); err != nil {
		return err
	}
	// 11. Image taxonomy
	if err := applyImageTaxonomy(c, dir, orgID); err != nil {
		return err
	}
	// 12. Playbooks
	if err := applyPlaybooks(c, dir, orgID, provisionAllowDrift); err != nil {
		return err
	}
	// 13. Test suites
	if err := applyTestSuites(c, dir, orgID); err != nil {
		return err
	}
	// 14. Router rules
	if err := applyRouterRules(c, dir, orgID); err != nil {
		return err
	}
	// 15. Widgets
	if err := applyWidgets(c, dir, orgID); err != nil {
		return err
	}
	// 16. Knowledge base
	if err := applyKnowledge(c, dir, orgID); err != nil {
		return err
	}
	// 17. Images manifest
	if err := applyImagesManifest(c, dir, orgID); err != nil {
		return err
	}

	// 18. Prompt templates — customer-tunable AI prompt bodies. Lives in
	// <dir>/prompts/*.txt; each file becomes one prompt_templates row keyed
	// by (org, filename-minus-.txt). Backs the no-deploy prompt-edit path.
	if err := applyPrompts(c, dir, orgID); err != nil {
		return err
	}

	// 19. Vanna training data — DDL, question/SQL examples and glossary terms
	// that ground the natural-language-to-SQL assistant. Lives in
	// <dir>/vanna-training.yaml; pushed last because it retrains the org's
	// vector index and should run after the dashboards it answers about exist.
	if err := applyVannaTraining(c, dir, orgID); err != nil {
		return err
	}

	// Exit codes for --strict mode
	if provisionStrict {
		if driftCount > 0 {
			fmt.Fprintf(os.Stderr, "\nprovision: strict mode — %d dashboard(s) would be updated (drift detected)\n", driftCount)
			os.Exit(2)
		}
		if c.WarningCount() > 0 {
			fmt.Fprintf(os.Stderr, "\nprovision: strict mode — %d warning(s) present\n", c.WarningCount())
			os.Exit(3)
		}
	}

	fmt.Printf("\nprovision: done (warnings=%d)\n", c.WarningCount())
	return nil
}
