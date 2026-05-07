package commands

import (
	"fmt"

	"github.com/spf13/cobra"
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

// Stubs — implemented in Task 3.
func runMCPInstall(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("not implemented")
}

func runMCPUninstall(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("not implemented")
}

func runMCPPrint(cmd *cobra.Command, args []string) error {
	// Same flow as install with --client print.
	flagMCPInstallClient = "print"
	return runMCPInstall(cmd, args)
}
