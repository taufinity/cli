package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/desktopconfig"
)

// Sibling subcommands to the existing mcpLoginCmd / mcpSwitchOrgCmd / mcpStatusCmd
// in commands/mcp.go. Those target Claude Code (.mcp.json); these target Claude
// Desktop (claude_desktop_config.json).

var (
	flagMCPInstallClient    string
	flagMCPInstallLabel     string
	flagMCPInstallForce     bool
	flagMCPInstallTransport string // "", "auto", "stdio", or "http"
)

var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Taufinity Studio as an MCP server in Claude Desktop",
	Long: `Install Taufinity Studio as an MCP server entry in Claude Desktop's config:
  macOS:   ~/Library/Application Support/Claude/claude_desktop_config.json
  Windows: %APPDATA%\Claude\claude_desktop_config.json

For Claude Code (project-level .mcp.json or ~/.claude.json) use 'taufinity mcp login' instead.

Use --client print to emit the JSON block to stdout without writing to disk.

By default this installs the stdio bridge shape for claude-desktop: Claude
Desktop launches the local taufinity CLI as a subprocess (taufinity mcp stdio),
which forwards JSON-RPC frames to Studio's /mcp endpoint and reads credentials
from disk at startup. No bearer token is written to the Claude Desktop config.

Global flags --org and --api-url are honored: their values are embedded into
the bridge subprocess args so Claude Desktop launches the CLI already scoped
to the right organization and Studio endpoint.

Pass --transport http to force the legacy HTTP shape (bearer embedded in the
config) — useful for Claude Desktop builds that support remote MCP natively.
--client print always emits HTTP, regardless of --transport.`,
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
	mcpInstallCmd.Flags().StringVar(&flagMCPInstallTransport, "transport", "",
		"MCP transport: stdio, http, or auto (default: stdio for claude-desktop, http for print)")

	mcpUninstallCmd.Flags().StringVar(&flagMCPInstallLabel, "label", "taufinity-studio", "Server entry name to remove")
}

// orgIDPattern matches the org identifiers we're willing to embed verbatim
// into stdio-bridge args. The CLI uses numeric IDs in practice; we also
// accept slugs (letters/digits/dash/underscore) for future-proofing. The
// restriction prevents accidental config corruption from quoted values in
// shell-config files and forecloses any future shell-out scenarios.
var orgIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func runMCPInstall(cmd *cobra.Command, args []string) error {
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated — run 'taufinity auth login' first")
	}

	label := flagMCPInstallLabel
	if label == "" {
		label = "taufinity-studio"
	}

	apiURL := strings.TrimRight(GetAPIURL(), "/")
	if strings.Contains(apiURL, "localhost") || strings.Contains(apiURL, "127.0.0.1") {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: API URL is localhost — installed config will only work for local Studio")
	}

	transport, err := resolveTransport(flagMCPInstallClient, flagMCPInstallTransport)
	if err != nil {
		return err
	}

	// --client=print is a neutral copy-paste artifact and always emits HTTP.
	if flagMCPInstallClient == "print" {
		entry, err := buildHTTPEntry(apiURL)
		if err != nil {
			return err
		}
		out := map[string]any{"mcpServers": map[string]any{label: entry}}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if flagMCPInstallClient != "claude-desktop" {
		return fmt.Errorf("unknown --client %q (use claude-desktop or print)", flagMCPInstallClient)
	}

	path := claudeDesktopConfigPath()
	if path == "" {
		path, err = desktopconfig.DefaultClaudeDesktopPath()
		if err != nil {
			return err
		}
	}

	// Build the server entry up front so we know the target transport before
	// the existence check (used to produce a clearer upgrade hint).
	var entry any
	switch transport {
	case "stdio":
		entry, err = buildStdioEntry(apiURL)
	case "http":
		entry, err = buildHTTPEntry(apiURL)
	}
	if err != nil {
		return err
	}

	if !flagMCPInstallForce {
		exists, _ := desktopconfig.HasServer(path, label)
		if exists {
			return fmt.Errorf("entry %q already exists in %s; pass --force to overwrite%s",
				label, path, upgradeHint(path, label, transport))
		}
	}

	if err := desktopconfig.UpsertServer(path, label, entry); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %q in %s\nRestart Claude Desktop to load the new server.\n", label, path)
	return nil
}

// resolveTransport picks the on-disk shape. The explicit --transport flag
// wins; otherwise the default depends on the client: stdio for claude-desktop
// (the only shape it reliably launches today), http for print (neutral).
func resolveTransport(client, override string) (string, error) {
	switch strings.ToLower(override) {
	case "stdio":
		return "stdio", nil
	case "http":
		return "http", nil
	case "", "auto":
		if client == "claude-desktop" {
			return "stdio", nil
		}
		return "http", nil
	default:
		return "", fmt.Errorf("invalid --transport %q: want stdio, http, or auto", override)
	}
}

// buildHTTPEntry produces a RemoteServer carrying the bearer token. Used for
// --client print and --transport http overrides.
func buildHTTPEntry(apiURL string) (desktopconfig.RemoteServer, error) {
	creds, err := auth.LoadCredentials()
	if err != nil {
		return desktopconfig.RemoteServer{}, fmt.Errorf("load credentials: %w", err)
	}
	token, err := creds.GetValidToken()
	if err != nil {
		return desktopconfig.RemoteServer{}, fmt.Errorf("%w — run 'taufinity auth login' to refresh", err)
	}
	return desktopconfig.RemoteServer{
		Type:    "http",
		URL:     apiURL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + token},
	}, nil
}

// buildStdioEntry produces a StdioServer pointing at this very binary as a
// bridge subprocess. No bearer token is embedded — the bridge reads
// credentials.json at startup. --org and a non-default --api-url are passed
// through as flags so the bridge runs already scoped.
func buildStdioEntry(apiURL string) (desktopconfig.StdioServer, error) {
	bin, err := taufinityExecutable()
	if err != nil {
		return desktopconfig.StdioServer{}, err
	}
	org := GetOrg()
	if org != "" && !orgIDPattern.MatchString(org) {
		return desktopconfig.StdioServer{}, fmt.Errorf("invalid org %q for stdio bridge args: expected numeric ID or slug matching %s", org, orgIDPattern.String())
	}
	return desktopconfig.StdioServer{
		Command: bin,
		Args:    buildStdioArgs(apiURL, org),
	}, nil
}

// buildStdioArgs renders the args Claude Desktop should pass when launching
// the CLI as a bridge. Only non-default flags are embedded so the resulting
// config stays minimal (omit --api-url when using prod, omit --org when no
// org override is set).
func buildStdioArgs(apiURL, org string) []string {
	var args []string
	if org != "" {
		args = append(args, "--org", org)
	}
	if apiURL != "" && apiURL != strings.TrimRight(DefaultAPIURL, "/") {
		args = append(args, "--api-url", apiURL)
	}
	args = append(args, "mcp", "stdio")
	return args
}

// upgradeHint returns a short message appended to the "already exists" error
// when the existing entry uses a transport different from what we were about
// to write — typically a legacy HTTP entry left behind by an older CLI run.
// Empty string when the file is unreadable, the entry isn't found, or the
// shapes match.
func upgradeHint(path, label, targetTransport string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	rawEntry, ok := doc.MCPServers[label]
	if !ok {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawEntry, &fields); err != nil {
		return ""
	}
	_, hasCmd := fields["command"]
	_, hasType := fields["type"]
	switch {
	case targetTransport == "stdio" && !hasCmd && hasType:
		return " (existing entry uses the legacy HTTP transport; rerun with --force to upgrade to the stdio bridge)"
	case targetTransport == "http" && hasCmd && !hasType:
		return " (existing entry uses the stdio bridge; rerun with --force to switch to HTTP)"
	}
	return ""
}

// taufinityExecutable returns the absolute, symlink-resolved path to the
// running CLI binary. Stdio-bridge entries embed this path so Claude Desktop
// can launch `taufinity mcp stdio` directly. Symlinks are resolved so a
// Homebrew upgrade that moves the keg doesn't silently break installed
// configs. Temporary paths (e.g. those produced by `go run`) are rejected
// because they vanish at session end. Override via TAUFINITY_BINARY_PATH for
// tests; the override bypasses resolution and the temp-path check.
func taufinityExecutable() (string, error) {
	if override := os.Getenv("TAUFINITY_BINARY_PATH"); override != "" {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve taufinity binary path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// Fall back to the unresolved path rather than failing — the binary
		// may live on a filesystem without symlink support.
		resolved = exe
	}
	if isTempPath(resolved) {
		return "", fmt.Errorf("taufinity binary resolves to a temporary path (%s); install to a stable location first (e.g. 'go install github.com/taufinity/cli/cmd/taufinity@latest' or Homebrew) before running 'mcp install'", resolved)
	}
	return resolved, nil
}

// isTempPath reports whether the given absolute path lives under a
// temporary-files prefix that won't survive a reboot or a `go run` exit.
func isTempPath(p string) bool {
	prefixes := []string{
		os.TempDir(),
		"/tmp",
		"/private/var/folders", // macOS `go run` exe path
		"/private/tmp",
	}
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		trimmed := strings.TrimRight(prefix, "/")
		if p == trimmed || strings.HasPrefix(p, trimmed+"/") {
			return true
		}
	}
	return false
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

