// provision_knowledge.go — provision support for knowledge files (price lists,
// templates, golden records).
//
// Calls POST /api/admin/knowledge-files/upsert (admin endpoint) which:
//   - matches existing rows by (org_id, name [, file_type]),
//   - short-circuits to NOOP if SHA256(content) == existing.Checksum,
//   - writes a v1 version snapshot on initial CREATE,
//   - tags the version row with X-Change-Source: provision (set by provision_client.go).
//
// Each YAML file under studio/knowledge-base/ becomes one knowledge file.
// `content_path` (preferred) loads from a sibling file on disk so big
// price lists don't bloat the YAML; `content` (inline) is supported for
// small payloads.
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// knowledgeFileConfig is the YAML shape for one knowledge file.
type knowledgeFileConfig struct {
	Name          string         `yaml:"name"`
	FileType      string         `yaml:"file_type,omitempty"`
	SourceType    string         `yaml:"source_type,omitempty"`
	Purpose       string         `yaml:"purpose,omitempty"`
	Tags          []string       `yaml:"tags,omitempty"`
	Content       string         `yaml:"content,omitempty"`
	ContentPath   string         `yaml:"content_path,omitempty"`
	ChangeSummary string         `yaml:"change_summary,omitempty"`
	Metadata      map[string]any `yaml:"metadata,omitempty"`
}

// knowledgeUpsertResponse mirrors the JSON returned by the admin upsert endpoint.
// action ∈ {created, updated, noop}.
type knowledgeUpsertResponse struct {
	ID       uint   `json:"id"`
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	FileType string `json:"file_type"`
	Action   string `json:"action"`
	Checksum string `json:"checksum"`
}

// applyKnowledge applies all knowledge files from the knowledge-base/ subdirectory.
func applyKnowledge(c *provisionClient, dir string, orgID uint) error {
	kd := filepath.Join(dir, "knowledge-base")
	if !fileExists(kd) {
		return nil
	}
	return provisionKnowledgeBase(c, kd, orgID)
}

// provisionKnowledgeBase walks every *.yaml file under dir, resolves
// content_path relative to each YAML's directory, and pushes one upsert
// per file. Aggregates a summary line at the end (created/updated/noop).
func provisionKnowledgeBase(c *provisionClient, dir string, orgID uint) error {
	entries, err := walkKnowledgeYAMLs(dir)
	if err != nil {
		return fmt.Errorf("knowledge: walk %s: %w", dir, err)
	}
	if len(entries) == 0 {
		fmt.Printf("provision: no knowledge files found under %s\n", dir)
		return nil
	}

	var created, updated, noop, skipped int
	for _, path := range entries {
		var cfg knowledgeFileConfig
		mustReadYAML(path, &cfg)
		if strings.TrimSpace(cfg.Name) == "" {
			return fmt.Errorf("knowledge file %s: name is required", path)
		}
		content, err := resolveKnowledgeContent(path, cfg)
		if err != nil {
			// Skip-not-fail policy: content_path that points at a missing
			// file (typically gitignored JSONs) is a recoverable state.
			if os.IsNotExist(err) || isContentPathMissing(err) {
				c.Warn("knowledge file %s: content file missing on disk — skipping. Run `provision kb-import --from <source-url>` to populate from another Studio.", path)
				skipped++
				continue
			}
			return fmt.Errorf("knowledge file %s: %w", path, err)
		}

		payload := map[string]interface{}{
			"org_id":         orgID,
			"title":          cfg.Name,
			"content":        content,
			"file_type":      cfg.FileType,
			"source_type":    cfg.SourceType,
			"purpose":        cfg.Purpose,
			"tags":           cfg.Tags,
			"metadata":       cfg.Metadata,
			"change_summary": cfg.ChangeSummary,
		}
		payloadBytes, _ := json.Marshal(payload)

		body, status, err := c.write("POST", "/admin/knowledge-files/upsert", payloadBytes)
		if err != nil || status >= 300 {
			return fmt.Errorf("knowledge upsert %q: status=%d err=%v body=%s",
				cfg.Name, status, err, provisionSummarize(body))
		}
		if c.dryRun {
			fmt.Printf("[dry-run] knowledge upsert %q (file_type=%q)\n", cfg.Name, cfg.FileType)
			continue
		}
		var resp knowledgeUpsertResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("knowledge upsert %q: parse response: %w (body=%s)", cfg.Name, err, provisionSummarize(body))
		}
		switch resp.Action {
		case "created":
			created++
			fmt.Printf("CREATE knowledge %q id=%d file_type=%q\n", resp.Name, resp.ID, resp.FileType)
		case "updated":
			updated++
			fmt.Printf("UPDATE knowledge %q id=%d file_type=%q\n", resp.Name, resp.ID, resp.FileType)
		case "noop":
			noop++
			fmt.Printf("NOOP   knowledge %q id=%d file_type=%q (content unchanged)\n", resp.Name, resp.ID, resp.FileType)
		default:
			fmt.Printf("?      knowledge %q action=%q\n", resp.Name, resp.Action)
		}
	}

	fmt.Printf("provision: knowledge summary: created=%d updated=%d noop=%d skipped=%d\n",
		created, updated, noop, skipped)
	return nil
}

// isContentPathMissing detects the error wrapped by resolveKnowledgeContent
// when os.ReadFile fails with ENOENT.
func isContentPathMissing(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return os.IsNotExist(pathErr)
	}
	return os.IsNotExist(err)
}

// walkKnowledgeYAMLs returns every *.yaml/*.yml file directly under dir,
// excluding files starting with `_` (reserved prefix for non-knowledge
// metadata files like _tombstones, _index).
func walkKnowledgeYAMLs(dir string) ([]string, error) {
	var out []string
	for _, ext := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(dir, ext))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			base := filepath.Base(m)
			if strings.HasPrefix(base, "_") {
				continue
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// resolveKnowledgeContent returns the content bytes for a knowledge file.
// Prefers content_path over inline content.
func resolveKnowledgeContent(yamlPath string, cfg knowledgeFileConfig) (string, error) {
	hasInline := cfg.Content != ""
	hasPath := cfg.ContentPath != ""
	if hasInline && hasPath {
		return "", fmt.Errorf("set either `content` or `content_path`, not both")
	}
	if !hasInline && !hasPath {
		return "", fmt.Errorf("set either `content` (inline) or `content_path` (file on disk)")
	}
	if hasInline {
		return cfg.Content, nil
	}
	resolved := cfg.ContentPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(yamlPath), resolved)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read content_path %s: %w", resolved, err)
	}
	return string(data), nil
}
