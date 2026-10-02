package desktopconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	path := ToolTogglesPath(configPath)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	var version int
	if err := json.Unmarshal(doc["v"], &version); err != nil || version != toolTogglesVersion {
		return 0, nil
	}
	var owners map[string][]string
	if err := json.Unmarshal(doc["owners"], &owners); err != nil {
		return 0, nil
	}

	prefix := "local:" + server + ":"
	removed := 0
	for owner, tools := range owners {
		kept := tools[:0:0]
		for _, t := range tools {
			if strings.HasPrefix(t, prefix) {
				removed++
				continue
			}
			kept = append(kept, t)
		}
		owners[owner] = kept
	}
	if removed == 0 {
		return 0, nil
	}

	encoded, err := json.Marshal(owners)
	if err != nil {
		return 0, err
	}
	doc["owners"] = encoded
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	if err := writeDocAtomic(path, out); err != nil {
		return 0, err
	}
	return removed, nil
}
