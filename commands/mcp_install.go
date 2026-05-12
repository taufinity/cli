package commands

import (
	"encoding/json"
	"fmt"
	"os"
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
	Short: "Install Taufinity Studio as an MCP server in supported clients",
	Long: `Install Taufinity Studio as an MCP server entry in one or more client
config files. Pass --client to pick a target:

  claude-desktop  ~/Library/Application Support/Claude/claude_desktop_config.json (macOS)
                  %APPDATA%\Claude\claude_desktop_config.json (Windows)
  claude-code     ~/.claude.json (all OSes; read by Claude Code CLI and IDE extensions)
  cursor          ~/.cursor/mcp.json (all OSes)
  vscode          VS Code's native MCP config (1.95+):
                  ~/Library/Application Support/Code/User/mcp.json (macOS)
                  ~/.config/Code/User/mcp.json (Linux)
                  %APPDATA%\Code\User\mcp.json (Windows)
  antigravity     Google's Antigravity IDE (best-effort path; override via
                  TAUFINITY_ANTIGRAVITY_CONFIG if needed)
  all             Install into every detected client (those whose config
                  directory already exists on disk)
  print           Emit the JSON block to stdout without writing anywhere

For Claude Code's project-level .mcp.json with a bearer token, use
'taufinity mcp login' instead.

By default this installs the stdio bridge shape: the client launches the
local taufinity CLI as a subprocess (taufinity mcp stdio), which forwards
JSON-RPC frames to Studio's /mcp endpoint and reads credentials from disk
at startup. No bearer token is written to the client config.

Global flags --org and --api-url are honored: their values are embedded
into the bridge subprocess args so the client launches the CLI already
scoped to the right organization and Studio endpoint.

Pass --transport http to force the legacy HTTP shape (bearer embedded in
the config) — useful for clients that support remote MCP natively.
--client print always emits HTTP, regardless of --transport.

Examples:
  taufinity mcp install                           # claude-desktop with stdio bridge
  taufinity mcp install --client cursor           # cursor only
  taufinity mcp install --client all              # every detected client
  taufinity --org 3 mcp install --client all \
        --label taufinity-voorpositiviteit         # all detected, pinned to org 3
  taufinity mcp install --client print            # preview the JSON, write nothing`,
	RunE: runMCPInstall,
}

var (
	flagMCPUninstallClient string
	flagMCPUninstallLabel  string
)

var mcpUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Taufinity Studio from a client's MCP config",
	Long: `Remove the named MCP server entry from one client's config.
Pass --client all to remove the entry from every detected client.

The --label value must match the one used at install time
(default: taufinity-studio).`,
	RunE: runMCPUninstall,
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

	mcpInstallCmd.Flags().StringVar(&flagMCPInstallClient, "client", "claude-desktop", "Client to install into: claude-desktop, claude-code, cursor, vscode, antigravity, all, print")
	mcpInstallCmd.Flags().StringVar(&flagMCPInstallLabel, "label", "taufinity-studio", "Server entry name in claude_desktop_config.json")
	mcpInstallCmd.Flags().BoolVar(&flagMCPInstallForce, "force", false, "Overwrite existing entry without prompting")
	mcpInstallCmd.Flags().StringVar(&flagMCPInstallTransport, "transport", "",
		"MCP transport: stdio, http, or auto (default: stdio for claude-desktop, http for print)")

	mcpUninstallCmd.Flags().StringVar(&flagMCPUninstallClient, "client", "claude-desktop", "Client to uninstall from: claude-desktop, claude-code, cursor, vscode, antigravity, all")
	mcpUninstallCmd.Flags().StringVar(&flagMCPUninstallLabel, "label", "taufinity-studio", "Server entry name to remove")
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

	// --client print is a neutral copy-paste artifact and always emits HTTP.
	// Reject an explicit --transport stdio rather than silently downgrading it
	// (the bridge JSON needs a real binary path; print has no client context).
	if flagMCPInstallClient == "print" {
		if strings.ToLower(flagMCPInstallTransport) == "stdio" {
			return fmt.Errorf("--client print emits HTTP only; --transport stdio is not supported for print (use a real client for stdio bridge install)")
		}
		entry, err := buildHTTPEntry(apiURL)
		if err != nil {
			return err
		}
		out := map[string]any{"mcpServers": map[string]any{label: entry}}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if flagMCPInstallClient == "all" {
		return runMCPInstallAll(cmd, apiURL, label)
	}

	client, ok := lookupClient(flagMCPInstallClient)
	if !ok {
		return fmt.Errorf("unknown --client %q (valid: %s)", flagMCPInstallClient, clientNamesString())
	}
	return installToClient(cmd, client, apiURL, label, flagMCPInstallForce)
}

// installToClient performs the install dance for one client: resolve path,
// build the right entry shape, check for an existing entry, write atomically,
// and print a friendly status line. Returns any error verbatim.
func installToClient(cmd *cobra.Command, client *mcpClient, apiURL, label string, force bool) error {
	transport, err := resolveTransport(client.name, flagMCPInstallTransport)
	if err != nil {
		return err
	}

	path, err := client.resolvePath()
	if err != nil {
		return fmt.Errorf("%s: resolve path: %w", client.name, err)
	}

	var entry any
	switch transport {
	case "stdio":
		entry, err = buildStdioEntry(apiURL)
	case "http":
		entry, err = buildHTTPEntry(apiURL)
	}
	if err != nil {
		return fmt.Errorf("%s: build entry: %w", client.name, err)
	}

	if !force {
		exists, _ := desktopconfig.HasServerInKey(path, client.serversKey, label)
		if exists {
			return fmt.Errorf("%s: entry %q already exists in %s; pass --force to overwrite%s",
				client.name, label, path, upgradeHintInKey(path, client.serversKey, label, transport))
		}
	}

	if err := desktopconfig.UpsertServerInKey(path, client.serversKey, label, entry); err != nil {
		return fmt.Errorf("%s: write %s: %w", client.name, path, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %q in %s (%s)\n", label, path, client.name)
	return nil
}

// runMCPInstallAll iterates every registered client, skipping ones not
// detected on disk (parent config dir absent), and aggregates results. Exits
// non-zero only when EVERY detected target failed — partial successes are
// reported but considered overall-success so a single missing client doesn't
// break the workflow.
func runMCPInstallAll(cmd *cobra.Command, apiURL, label string) error {
	var installed, skipped []string
	type installErr struct {
		name string
		err  error
	}
	var errs []installErr

	for i := range mcpClientsList {
		c := &mcpClientsList[i]
		if !detectInstalled(c) {
			skipped = append(skipped, c.name+" (not detected)")
			continue
		}
		if err := installToClient(cmd, c, apiURL, label, flagMCPInstallForce); err != nil {
			errs = append(errs, installErr{name: c.name, err: err})
			continue
		}
		installed = append(installed, c.name)
	}

	// Summary on stderr so install lines stay clean on stdout.
	fmt.Fprintln(cmd.ErrOrStderr())
	fmt.Fprintln(cmd.ErrOrStderr(), "Summary:")
	if len(installed) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "  Installed: %s\n", strings.Join(installed, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "  Skipped:   %s\n", strings.Join(skipped, ", "))
	}
	for _, e := range errs {
		fmt.Fprintf(cmd.ErrOrStderr(), "  Failed:    %s — %v\n", e.name, e.err)
	}
	if len(installed) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Restart each affected client to load the new server.")
	}

	// Non-zero only when there's nothing to show for the run.
	if len(installed) == 0 && len(errs) > 0 {
		return fmt.Errorf("all targets failed")
	}
	if len(installed) == 0 && len(skipped) > 0 {
		return fmt.Errorf("no clients detected — install one of: %s", strings.Join(allClientNames(), ", "))
	}
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

// upgradeHintInKey returns a short message appended to the "already exists"
// error when the existing entry uses a transport different from what we were
// about to write — typically a legacy HTTP entry left behind by an older CLI
// run. Empty string when the file is unreadable, the entry isn't found, or
// the shapes match. The key parameter is "mcpServers" for most clients and
// "servers" for VS Code.
func upgradeHintInKey(path, key, label, targetTransport string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	rawServers, ok := doc[key]
	if !ok {
		return ""
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return ""
	}
	rawEntry, ok := servers[label]
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

// taufinityExecutable returns the path to the running CLI binary, as
// reported by the OS — symlinks are intentionally NOT resolved. For
// Homebrew installs the stable path is the symlink (e.g.
// /opt/homebrew/bin/taufinity); resolving it would pin to a specific
// keg (.../Cellar/taufinity/X.Y.Z/bin/...) that disappears on
// 'brew upgrade'. The symlink itself is what we want embedded in
// Claude Desktop's config so the bridge keeps working across upgrades.
//
// Temporary paths (e.g. those produced by 'go run', which lands in
// /private/var/folders or os.TempDir) are rejected because they vanish
// at session end, leaving Claude Desktop with a broken install.
// Override via TAUFINITY_BINARY_PATH for tests; the override bypasses
// the temp-path check.
func taufinityExecutable() (string, error) {
	if override := os.Getenv("TAUFINITY_BINARY_PATH"); override != "" {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve taufinity binary path: %w", err)
	}
	if isTempPath(exe) {
		return "", fmt.Errorf("taufinity binary resolves to a temporary path (%s); install to a stable location first (e.g. 'go install github.com/taufinity/cli/cmd/taufinity@latest' or Homebrew) before running 'mcp install'", exe)
	}
	return exe, nil
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
	label := flagMCPUninstallLabel
	if label == "" {
		label = "taufinity-studio"
	}

	if flagMCPUninstallClient == "all" {
		var removed, skipped []string
		for i := range mcpClientsList {
			c := &mcpClientsList[i]
			path, err := c.resolvePath()
			if err != nil {
				skipped = append(skipped, c.name+" (no path)")
				continue
			}
			// Only touch files that actually exist — RemoveServer would
			// otherwise create an empty config dir as a side effect.
			if _, err := os.Stat(path); err != nil {
				skipped = append(skipped, c.name+" (no config)")
				continue
			}
			if err := desktopconfig.RemoveServerInKey(path, c.serversKey, label); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %v\n", c.name, err)
				continue
			}
			removed = append(removed, c.name)
		}
		fmt.Fprintln(cmd.ErrOrStderr())
		fmt.Fprintln(cmd.ErrOrStderr(), "Summary:")
		if len(removed) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "  Removed:   %s\n", strings.Join(removed, ", "))
		}
		if len(skipped) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "  Skipped:   %s\n", strings.Join(skipped, ", "))
		}
		return nil
	}

	client, ok := lookupClient(flagMCPUninstallClient)
	if !ok {
		return fmt.Errorf("unknown --client %q (valid: %s)", flagMCPUninstallClient, clientNamesString())
	}
	path, err := client.resolvePath()
	if err != nil {
		return err
	}
	if err := desktopconfig.RemoveServerInKey(path, client.serversKey, label); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %q from %s (%s)\n", label, path, client.name)
	return nil
}

func runMCPPrint(cmd *cobra.Command, args []string) error {
	// runMCPInstall reads the package-level flag; save/restore so a 'mcp print'
	// invocation in the same process (or test binary) doesn't leak "print" into
	// a subsequent 'mcp install' invocation.
	prev := flagMCPInstallClient
	defer func() { flagMCPInstallClient = prev }()
	flagMCPInstallClient = "print"
	return runMCPInstall(cmd, args)
}


