package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/auth"
)

const (
	mcpConfigFile       = ".mcp.json"
	mcpGlobalConfigFile = "mcp.json"
	claudeConfigDir     = ".claude"
)

var (
	flagMCPTarget string
	flagMCPInit   bool
	flagMCPGlobal bool
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP (Claude Code) integration",
	Long: `Configure .mcp.json for Claude Code MCP server integration, or run a
stdio MCP bridge for stdio-only clients.

The login/switch-org/status subcommands update the .mcp.json file in your
project directory with credentials from 'taufinity auth login'.

The 'stdio' subcommand runs a local bridge that forwards JSON-RPC frames
over stdio to Studio's remote /mcp endpoint, for older Claude Desktop
versions, mcp-inspector, or custom clients that only speak stdio.

Use --global to target ~/.claude/mcp.json (user-level config) instead
of the project-level .mcp.json.`,
}

var mcpLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Update MCP config with current CLI credentials",
	Long: `Updates the MCP server configuration with your current CLI auth token.

This reads your credentials from 'taufinity auth login' and writes
them into .mcp.json so Claude Code can authenticate with the API.

By default updates the 'taufinity-prod' server entry in the nearest
.mcp.json. Use --global to update ~/.claude/mcp.json instead.

Use --init to create a new config file if none exists.`,
	RunE: runMCPLogin,
}

var mcpSwitchOrgCmd = &cobra.Command{
	Use:   "switch-org ID",
	Short: "Switch organization in MCP config",
	Long: `Updates the X-Organization-ID header in .mcp.json.

This changes which organization the MCP tools operate on
without needing to re-authenticate.`,
	Args: cobra.ExactArgs(1),
	RunE: runMCPSwitchOrg,
}

var mcpStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current MCP configuration",
	Long:  `Display the current .mcp.json server entries and their auth status.`,
	RunE:  runMCPStatus,
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.AddCommand(mcpLoginCmd)
	mcpCmd.AddCommand(mcpSwitchOrgCmd)
	mcpCmd.AddCommand(mcpStatusCmd)

	mcpCmd.PersistentFlags().StringVar(&flagMCPTarget, "target", "taufinity-prod", "MCP server name to update")
	mcpCmd.PersistentFlags().BoolVar(&flagMCPGlobal, "global", false, "Use ~/.claude/mcp.json instead of project .mcp.json")

	mcpLoginCmd.Flags().BoolVar(&flagMCPInit, "init", false, "Create .mcp.json if it doesn't exist")
}

// mcpConfig represents the .mcp.json file structure.
type mcpConfig struct {
	Servers map[string]mcpServer `json:"mcpServers"`
}

// mcpServer represents a single MCP server entry.
type mcpServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// globalMCPConfigPath returns ~/.claude/mcp.json.
func globalMCPConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(home, claudeConfigDir, mcpGlobalConfigFile), nil
}

// findMCPConfig walks up from cwd to find .mcp.json.
func findMCPConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	for {
		path := filepath.Join(dir, mcpConfigFile)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found (searched from current directory to root)\nUse --init to create one, or --global for ~/.claude/mcp.json", mcpConfigFile)
		}
		dir = parent
	}
}

// resolveMCPConfigPath returns the path to read/write based on --global flag.
func resolveMCPConfigPath() (string, error) {
	if flagMCPGlobal {
		p, err := globalMCPConfigPath()
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("no global config at %s\nUse 'taufinity mcp login --global --init' to create one", p)
		}
		return p, nil
	}
	return findMCPConfig()
}

// loadMCPConfig reads and parses an MCP config file.
func loadMCPConfig(path string) (*mcpConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.Servers == nil {
		cfg.Servers = make(map[string]mcpServer)
	}

	return &cfg, nil
}

// backupFile creates a backup of path before overwriting.
// Uses .bak extension, adding a timestamp if .bak already exists.
func backupFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil // Nothing to back up
	}

	bakPath := path + ".bak"
	if _, err := os.Stat(bakPath); err == nil {
		// .bak already exists — use timestamp
		bakPath = path + "." + time.Now().Format("20060102-150405") + ".bak"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read for backup: %w", err)
	}

	if err := os.WriteFile(bakPath, data, 0600); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}

	return nil
}

// saveMCPConfig writes the config back to disk, preserving original file permissions.
// Creates a backup of the existing file before overwriting.
func saveMCPConfig(path string, cfg *mcpConfig) error {
	if err := backupFile(path); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	data = append(data, '\n')

	// Default to 0600 (contains auth tokens). If the file already exists,
	// preserve its permissions but enforce a maximum of 0600 — never allow
	// group or world read even if the existing file was more permissive.
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm() & 0600
	}

	// Ensure parent directory exists (needed for --global on fresh systems)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// initMCPConfig creates a new MCP config with a single server entry.
func initMCPConfig(target, token string) *mcpConfig {
	return &mcpConfig{
		Servers: map[string]mcpServer{
			target: {
				Type: "http",
				URL:  GetAPIURL() + "/mcp",
				Headers: map[string]string{
					"X-API-Key": token,
				},
			},
		},
	}
}

func runMCPLogin(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	creds, err := auth.LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}

	token, err := creds.GetValidToken()
	if err != nil {
		return fmt.Errorf("run 'taufinity auth login' to re-authenticate: %w", err)
	}

	// Resolve target path
	path, err := resolveMCPConfigPath()
	if err != nil {
		if !flagMCPInit {
			return err
		}
		// --init: create new file
		if flagMCPGlobal {
			path, err = globalMCPConfigPath()
		} else {
			dir, dirErr := os.Getwd()
			if dirErr != nil {
				return fmt.Errorf("get working directory: %w", dirErr)
			}
			path = filepath.Join(dir, mcpConfigFile)
			err = nil
		}
		if err != nil {
			return err
		}

		cfg := initMCPConfig(flagMCPTarget, token)
		if err := saveMCPConfig(path, cfg); err != nil {
			return err
		}

		Print("Created %s\n", path)
		printTokenInfo(creds)
		return nil
	}

	// Load and update existing config
	cfg, err := loadMCPConfig(path)
	if err != nil {
		return err
	}

	server, exists := cfg.Servers[flagMCPTarget]
	if !exists {
		// Auto-add new server entry to existing config
		server = mcpServer{
			Type: "http",
			URL:  GetAPIURL() + "/mcp",
			Headers: map[string]string{
				"X-API-Key": token,
			},
		}
	} else {
		if server.Headers == nil {
			server.Headers = make(map[string]string)
		}
		server.Headers["X-API-Key"] = token
	}

	cfg.Servers[flagMCPTarget] = server

	if err := saveMCPConfig(path, cfg); err != nil {
		return err
	}

	if exists {
		Print("Updated %s in %s\n", flagMCPTarget, path)
	} else {
		Print("Added %s to %s\n", flagMCPTarget, path)
	}
	printTokenInfo(creds)
	return nil
}

func runMCPSwitchOrg(cmd *cobra.Command, args []string) error {
	orgID := args[0]

	if _, err := strconv.Atoi(orgID); err != nil {
		return fmt.Errorf("organization ID must be a number, got %q", orgID)
	}

	path, err := resolveMCPConfigPath()
	if err != nil {
		return err
	}

	cfg, err := loadMCPConfig(path)
	if err != nil {
		return err
	}

	server, exists := cfg.Servers[flagMCPTarget]
	if !exists {
		return fmt.Errorf("server %q not found in %s (available: %s)", flagMCPTarget, path, serverNames(cfg))
	}

	if server.Headers == nil {
		server.Headers = make(map[string]string)
	}

	server.Headers["X-Organization-ID"] = orgID
	cfg.Servers[flagMCPTarget] = server

	if err := saveMCPConfig(path, cfg); err != nil {
		return err
	}

	Print("Switched %s to organization %s in %s\n", flagMCPTarget, orgID, path)
	return nil
}

func runMCPStatus(cmd *cobra.Command, args []string) error {
	path, err := resolveMCPConfigPath()
	if err != nil {
		return err
	}

	cfg, err := loadMCPConfig(path)
	if err != nil {
		return err
	}

	Print("Config: %s\n\n", path)

	for name, server := range cfg.Servers {
		Print("  %s\n", name)
		Print("    URL: %s\n", server.URL)

		if orgID, ok := server.Headers["X-Organization-ID"]; ok {
			Print("    Organization: %s\n", orgID)
		}

		if _, ok := server.Headers["X-API-Key"]; ok {
			Print("    Auth: Configured\n")
		} else if _, ok := server.Headers["Authorization"]; ok {
			Print("    Auth: Configured\n")
		} else {
			Print("    Auth: Not configured\n")
		}

		Print("\n")
	}

	return nil
}

func printTokenInfo(creds *auth.Credentials) {
	Print("Token from: %s", creds.Email)
	if creds.OrganizationName != "" {
		Print(" (%s)", creds.OrganizationName)
	}
	Print("\n")
	Print("Expires: %s\n", creds.ExpiresAt.Format("2006-01-02 15:04"))
}

// serverNames returns a comma-separated list of server names for error messages.
func serverNames(cfg *mcpConfig) string {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
