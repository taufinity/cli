package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/taufinity/cli/internal/desktopconfig"
)

// mcpClient describes one supported install target. The registry below holds
// one entry per client; runMCPInstall and runMCPUninstall iterate it for
// --client all and dispatch by name otherwise.
//
// Adding a new client: append a new entry to mcpClientsList. The path
// function should return the per-OS config path and may return ErrUnsupported
// to signal "no path on this OS, skip cleanly."
type mcpClient struct {
	// name is the value users pass to --client (lowercase, hyphen-separated).
	name string

	// description is shown in help text and summaries.
	description string

	// serversKey is the top-level JSON key under which MCP server entries
	// live. Almost everything uses "mcpServers"; VS Code uses "servers".
	serversKey string

	// resolvePath returns the absolute path to the config file. Errors
	// (e.g. unsupported OS, missing HOME) propagate. Returning a path
	// whose parent directory doesn't exist is legal — UpsertServer will
	// MkdirAll on write.
	resolvePath func() (string, error)

	// detect, when non-nil, overrides the default "parent dir exists"
	// detection. Used by clients whose config lives directly in $HOME
	// (e.g. claude-code's ~/.claude.json) — for those, parent-dir
	// existence is meaningless since HOME always exists, so we check
	// for the file itself instead.
	detect func() bool
}

// mcpClientsList is the ordered registry of supported install targets.
// Order is the order they appear in summaries.
var mcpClientsList = []mcpClient{
	{
		name:        "claude-desktop",
		description: "Anthropic's Claude Desktop app",
		serversKey:  desktopconfig.DefaultServersKey,
		resolvePath: claudeDesktopPath,
	},
	{
		name:        "claude-code",
		description: "Claude Code (CLI + IDE extensions, global config)",
		serversKey:  desktopconfig.DefaultServersKey,
		resolvePath: claudeCodePath,
		detect:      claudeCodeDetect,
	},
	{
		name:        "cursor",
		description: "Cursor IDE (global MCP config)",
		serversKey:  desktopconfig.DefaultServersKey,
		resolvePath: cursorPath,
	},
	{
		name:        "vscode",
		description: "VS Code native MCP (1.95+), user-level",
		serversKey:  "servers",
		resolvePath: vscodePath,
	},
	{
		name:        "antigravity",
		description: "Antigravity (Google's agentic IDE)",
		serversKey:  desktopconfig.DefaultServersKey,
		resolvePath: antigravityPath,
	},
}

// lookupClient finds a client by name. Returns nil + false if unknown.
func lookupClient(name string) (*mcpClient, bool) {
	for i := range mcpClientsList {
		if mcpClientsList[i].name == name {
			return &mcpClientsList[i], true
		}
	}
	return nil, false
}

// allClientNames returns the registry names in registry order. Used in error
// messages and `--help` rendering.
func allClientNames() []string {
	out := make([]string, len(mcpClientsList))
	for i, c := range mcpClientsList {
		out[i] = c.name
	}
	return out
}

// clientNamesString formats the registry names for a human-readable error.
func clientNamesString() string {
	names := allClientNames()
	sort.Strings(names)
	return fmt.Sprintf("%v, all, print", names)
}

// --- per-client path resolvers ---
//
// Convention: each returns the absolute path to the JSON config file. They
// do not check whether the file or its parent dir exist — detection of
// "is this client actually installed" is done by detectInstalled() below,
// which checks the parent dir.

// claudeDesktopPath returns the per-OS Claude Desktop config path, honoring
// the TAUFINITY_DESKTOP_CONFIG env-var override (useful for tests).
func claudeDesktopPath() (string, error) {
	if override := os.Getenv("TAUFINITY_DESKTOP_CONFIG"); override != "" {
		return override, nil
	}
	return desktopconfig.DefaultClaudeDesktopPath()
}

// claudeCodePath: ~/.claude.json on all OSes. Same file the Claude Code CLI
// and the official VS Code extension both read for MCP server entries.
func claudeCodePath() (string, error) {
	if override := os.Getenv("TAUFINITY_CLAUDE_CODE_CONFIG"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// cursorPath: ~/.cursor/mcp.json on all OSes (global config). Per-project
// .cursor/mcp.json is out of scope — the global config is the simpler default.
func cursorPath() (string, error) {
	if override := os.Getenv("TAUFINITY_CURSOR_CONFIG"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

// vscodePath: VS Code's user-level MCP config. Per the 1.95+ docs:
//
//	macOS:   ~/Library/Application Support/Code/User/mcp.json
//	Linux:   ~/.config/Code/User/mcp.json
//	Windows: %APPDATA%\Code\User\mcp.json
//
// The Insiders build uses "Code - Insiders" instead of "Code"; we target
// the stable build by default and let users override via env-var.
func vscodePath() (string, error) {
	if override := os.Getenv("TAUFINITY_VSCODE_CONFIG"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json"), nil
	case "linux":
		return filepath.Join(home, ".config", "Code", "User", "mcp.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA not set")
		}
		return filepath.Join(appData, "Code", "User", "mcp.json"), nil
	default:
		return "", fmt.Errorf("vscode: unsupported OS %s", runtime.GOOS)
	}
}

// antigravityPath: best-effort guess for Google's Antigravity IDE. The tool
// is in alpha and has shifted layouts already; provide an env-var escape
// hatch so users can point us at the right file if/when the path changes.
//
//	macOS:   ~/Library/Application Support/Antigravity/mcp.json
//	Linux:   ~/.config/Antigravity/mcp.json
//	Windows: %APPDATA%\Antigravity\mcp.json
func antigravityPath() (string, error) {
	if override := os.Getenv("TAUFINITY_ANTIGRAVITY_CONFIG"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Antigravity", "mcp.json"), nil
	case "linux":
		return filepath.Join(home, ".config", "Antigravity", "mcp.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA not set")
		}
		return filepath.Join(appData, "Antigravity", "mcp.json"), nil
	default:
		return "", fmt.Errorf("antigravity: unsupported OS %s", runtime.GOOS)
	}
}

// detectInstalled returns true if the config file's PARENT directory exists.
// That's our cheap proxy for "has this client ever been launched on this
// machine" — every supported client creates its config directory on first
// launch. False positives (dir exists but app uninstalled) are harmless: we
// write a config that's never read.
//
// A per-client detect override (mcpClient.detect) wins when set, for clients
// like claude-code whose config lives directly in $HOME — checking "does the
// parent dir exist" is meaningless when the parent IS HOME.
func detectInstalled(c *mcpClient) bool {
	if c.detect != nil {
		return c.detect()
	}
	path, err := c.resolvePath()
	if err != nil {
		return false
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// claudeCodeDetect returns true when ~/.claude.json exists — Claude Code
// creates this file on first session start to persist conversation history
// and settings, so its presence is a reliable signal the user has at least
// launched Claude Code once. A missing file means the user hasn't used
// Claude Code on this machine; --client all should skip it rather than
// silently materialise a config the user didn't ask for.
func claudeCodeDetect() bool {
	path, err := claudeCodePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
