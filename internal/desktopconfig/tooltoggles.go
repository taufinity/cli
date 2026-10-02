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
// Claude Desktop records the tools a user switched off per MCP server.
const ToolTogglesFile = "mcp-user-tool-toggles.json"

// toolTogglesVersion is the only layout of ToolTogglesFile this package
// understands: {"v": 3, "owners": {"<account>": ["local:<server>:<tool>", ...]}}.
// Any other version is left untouched rather than guessed at.
const toolTogglesVersion = 3

// ToolTogglesPath returns the toggles file that belongs to the Claude Desktop
// config at configPath.
func ToolTogglesPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ToolTogglesFile)
}

// ClearToolToggles re-enables every tool of the named local server that
// Claude Desktop has recorded as switched off, for every account in the file.
// It returns how many entries were removed.
//
// Why this exists: the list is keyed by server name and tool name, and it
// survives reinstalls and renames. A server that was once installed with most
// tools switched off keeps that state, so Studio later appears to be missing
// tools such as query_insights while the server itself serves all of them.
// Only tools added after the switch-off show up, which makes the cause hard
// to spot from inside a chat.
//
// A missing file, an empty file, or a layout other than v3 is not an error:
// nothing is changed and 0 is returned. Unknown top-level keys are preserved.
func ClearToolToggles(configPath, server string) (int, error) {
	t, err := loadToolToggles(configPath)
	if err != nil || t == nil {
		return 0, err
	}
	prefix := toggleKeyPrefix(server)
	removed := 0
	for owner, tools := range t.owners {
		kept := tools[:0:0]
		for _, name := range tools {
			if strings.HasPrefix(name, prefix) {
				removed++
				continue
			}
			kept = append(kept, name)
		}
		t.owners[owner] = kept
	}
	if removed == 0 {
		return 0, nil
	}
	if err := t.save(); err != nil {
		return 0, err
	}
	return removed, nil
}

// CountToolToggles reports how many tools of the named local server Claude
// Desktop has recorded as switched off, summed over all accounts. It reads
// the same layouts ClearToolToggles changes, so the two cannot disagree.
func CountToolToggles(configPath, server string) (int, error) {
	t, err := loadToolToggles(configPath)
	if err != nil || t == nil {
		return 0, err
	}
	prefix := toggleKeyPrefix(server)
	n := 0
	for _, tools := range t.owners {
		for _, name := range tools {
			if strings.HasPrefix(name, prefix) {
				n++
			}
		}
	}
	return n, nil
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
		base := strings.TrimSuffix(filepath.Base(command), ".exe")
		if base != "taufinity" {
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

func toggleKeyPrefix(server string) string { return "local:" + server + ":" }

// toolToggles is a parsed v3 toggles file. Unknown top-level keys are kept in
// doc so save writes them back unchanged.
type toolToggles struct {
	path   string
	doc    map[string]json.RawMessage
	owners map[string][]string
}

// loadToolToggles returns nil without an error when there is nothing this
// package should touch: no file, an empty file, or a layout other than v3.
func loadToolToggles(configPath string) (*toolToggles, error) {
	path := ToolTogglesPath(configPath)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var version int
	if err := json.Unmarshal(doc["v"], &version); err != nil || version != toolTogglesVersion {
		return nil, nil
	}
	var owners map[string][]string
	if err := json.Unmarshal(doc["owners"], &owners); err != nil {
		return nil, nil
	}
	return &toolToggles{path: path, doc: doc, owners: owners}, nil
}

func (t *toolToggles) save() error {
	encoded, err := json.Marshal(t.owners)
	if err != nil {
		return err
	}
	t.doc["owners"] = encoded
	out := make(map[string]any, len(t.doc))
	for k, v := range t.doc {
		out[k] = v
	}
	return writeDocAtomic(t.path, out)
}
