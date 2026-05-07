package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/desktopconfig"
)

// Sibling subcommands to the existing mcpLoginCmd / mcpSwitchOrgCmd / mcpStatusCmd
// in commands/mcp.go. Those target Claude Code (.mcp.json); these target Claude
// Desktop (claude_desktop_config.json).

var (
	flagMCPInstallClient string
	flagMCPInstallLabel  string
	flagMCPInstallForce  bool
)

var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Taufinity Studio as an MCP server in Claude Desktop",
	Long: `Reads your bearer token via 'taufinity auth token' and writes a server entry
to Claude Desktop's config file:
  macOS:   ~/Library/Application Support/Claude/claude_desktop_config.json
  Windows: %APPDATA%\Claude\claude_desktop_config.json

For Claude Code (project-level .mcp.json or ~/.claude.json) use 'taufinity mcp login' instead.

Use --client print to emit the JSON block to stdout without writing to disk.

The token is the same one 'taufinity auth login' issued — no new key is minted.
Revoke from the Studio admin UI if compromised.`,
	RunE: runMCPInstall,
}

var mcpUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Taufinity Studio from Claude Desktop's config",
	RunE:  runMCPUninstall,
}

var mcpPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the Claude Desktop server entry as JSON without writing it",
	RunE:  runMCPPrint,
}

func init() {
	mcpCmd.AddCommand(mcpInstallCmd)
	mcpCmd.AddCommand(mcpUninstallCmd)
	mcpCmd.AddCommand(mcpPrintCmd)

	mcpInstallCmd.Flags().StringVar(&flagMCPInstallClient, "client", "claude-desktop", "Client to install into: claude-desktop, print")
	mcpInstallCmd.Flags().StringVar(&flagMCPInstallLabel, "label", "taufinity-studio", "Server entry name in claude_desktop_config.json")
	mcpInstallCmd.Flags().BoolVar(&flagMCPInstallForce, "force", false, "Overwrite existing entry without prompting")

	mcpUninstallCmd.Flags().StringVar(&flagMCPInstallLabel, "label", "taufinity-studio", "Server entry name to remove")
}

func runMCPInstall(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}
	creds, err := auth.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	token, err := creds.GetValidToken()
	if err != nil {
		return fmt.Errorf("%w — run 'taufinity auth login' to refresh", err)
	}

	apiURL := GetAPIURL()
	server := desktopconfig.RemoteServer{
		Type: "http",
		URL:  strings.TrimRight(apiURL, "/") + "/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer " + token,
		},
	}

	label := flagMCPInstallLabel
	if label == "" {
		label = "taufinity-studio"
	}

	switch flagMCPInstallClient {
	case "print":
		out := map[string]any{"mcpServers": map[string]any{label: server}}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)

	case "claude-desktop":
		path := claudeDesktopConfigPath()
		if path == "" {
			path, err = desktopconfig.DefaultClaudeDesktopPath()
			if err != nil {
				return err
			}
		}
		if !flagMCPInstallForce {
			if existing, _ := desktopconfig.HasServer(path, label); existing {
				return fmt.Errorf("entry %q already exists in %s; pass --force to overwrite", label, path)
			}
		}
		if err := desktopconfig.UpsertServer(path, label, server); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Installed %q in %s\nRestart Claude Desktop to load the new server.\n", label, path)
		return nil

	default:
		return fmt.Errorf("unknown --client %q (use claude-desktop or print)", flagMCPInstallClient)
	}
}

func runMCPUninstall(cmd *cobra.Command, args []string) error {
	label := flagMCPInstallLabel
	if label == "" {
		label = "taufinity-studio"
	}
	path := claudeDesktopConfigPath()
	if path == "" {
		var err error
		path, err = desktopconfig.DefaultClaudeDesktopPath()
		if err != nil {
			return err
		}
	}
	if err := desktopconfig.RemoveServer(path, label); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %q from %s\n", label, path)
	return nil
}

func runMCPPrint(cmd *cobra.Command, args []string) error {
	flagMCPInstallClient = "print"
	return runMCPInstall(cmd, args)
}

// claudeDesktopConfigPath returns the path override from the env var, or empty
// for the per-OS default. The env var is mainly for tests; users should rely
// on the default path.
func claudeDesktopConfigPath() string {
	return os.Getenv("TAUFINITY_DESKTOP_CONFIG")
}
