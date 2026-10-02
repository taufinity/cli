package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/taufinity/cli/internal/config"
)

// toolCatalog remembers which Studio tools only read data, as Studio marks
// them (readOnlyHint). The bridge records it from the tools/list response
// Claude Desktop requests at startup, so the repair prompt can tell a
// problem (a read tool switched off, e.g. query_insights) from a choice
// (write or admin tools left off) without a request of its own.
type toolCatalog struct {
	UpdatedAt time.Time `json:"updated_at"`
	ReadOnly  []string  `json:"read_only"`
}

func toolCatalogPath() string { return filepath.Join(config.Dir(), "mcp-tools.json") }

// recordToolCatalog stores the read-only tools from a tools/list result.
// Only a complete, single-page list is recorded: a partial page would make
// the missing tools look like write tools. Best-effort; errors are ignored.
func recordToolCatalog(result json.RawMessage) {
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnlyHint *bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &list); err != nil || list.NextCursor != "" || len(list.Tools) == 0 {
		return
	}
	cat := toolCatalog{UpdatedAt: time.Now(), ReadOnly: []string{}}
	for _, t := range list.Tools {
		if t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint {
			cat.ReadOnly = append(cat.ReadOnly, t.Name)
		}
	}
	raw, err := json.Marshal(cat)
	if err != nil {
		return
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return
	}
	tmp := toolCatalogPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, toolCatalogPath())
}

// loadReadOnlyTools returns the recorded read-only tool names, or nil when
// no catalog has been recorded yet.
func loadReadOnlyTools() map[string]bool {
	raw, err := os.ReadFile(toolCatalogPath())
	if err != nil {
		return nil
	}
	var cat toolCatalog
	if json.Unmarshal(raw, &cat) != nil {
		return nil
	}
	set := make(map[string]bool, len(cat.ReadOnly))
	for _, n := range cat.ReadOnly {
		set[n] = true
	}
	return set
}
