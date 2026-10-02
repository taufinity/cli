package desktopconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ToolTogglesFile is the file, next to claude_desktop_config.json, in which
// Claude Desktop mirrors the tools a user switched off per MCP server.
//
// It is read-only for us. Desktop derives it from the claude.ai app's own
// storage and rewrites it on every start, so editing it changes nothing: the
// tools can only be switched on in Customize → Connectors. Reading it is
// reliable, because it follows that setting.
const ToolTogglesFile = "mcp-user-tool-toggles.json"

// toolTogglesVersion is the only layout of ToolTogglesFile this package
// understands: {"v": 3, "owners": {"<account>": ["local:<server>:<tool>", ...]}}.
// Any other version reads as "nothing switched off" rather than a guess.
const toolTogglesVersion = 3

// ToolTogglesPath returns the toggles file that belongs to the Claude Desktop
// config at configPath.
func ToolTogglesPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ToolTogglesFile)
}

// SwitchedOffTools returns the sorted, de-duplicated names of the tools of
// the named local server that Claude Desktop has switched off, over all
// accounts in the file. A missing file, an empty file, or a layout other than
// v3 yields no tools and no error.
func SwitchedOffTools(configPath, server string) ([]string, error) {
	path := ToolTogglesPath(configPath)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(raw) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		V      json.RawMessage     `json:"v"`
		Owners map[string][]string `json:"owners"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var version int
	if err := json.Unmarshal(doc.V, &version); err != nil || version != toolTogglesVersion {
		return nil, nil
	}

	prefix := "local:" + server + ":"
	seen := map[string]bool{}
	var tools []string
	for _, keys := range doc.Owners {
		for _, k := range keys {
			name, ok := strings.CutPrefix(k, prefix)
			if ok && !seen[name] {
				seen[name] = true
				tools = append(tools, name)
			}
		}
	}
	sort.Strings(tools)
	return tools, nil
}

// BridgeServers returns the names of the servers in the Claude Desktop config
// at configPath that launch a taufinity stdio bridge, whatever label they were
// installed under. A customer may have renamed the entry, so matching on the
// command is the only reliable way to find ours.
func BridgeServers(configPath string) ([]string, error) {
	doc, err := readDoc(configPath)
	if err != nil {
		return nil, err
	}
	servers, _ := doc[DefaultServersKey].(map[string]any)
	var names []string
	for name, raw := range servers {
		entry, _ := raw.(map[string]any)
		command, _ := entry["command"].(string)
		if strings.TrimSuffix(filepath.Base(command), ".exe") != "taufinity" {
			continue
		}
		args, _ := entry["args"].([]any)
		for _, a := range args {
			if a == "stdio" {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
