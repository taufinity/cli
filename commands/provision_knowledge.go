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
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// knowledgeFileConfig is the YAML shape for one knowledge file, or — when
// ContentGlob is set — a template for many knowledge files generated from
// a glob match set.
type knowledgeFileConfig struct {
	Name          string         `yaml:"name"`
	FileType      string         `yaml:"file_type,omitempty"`
	SourceType    string         `yaml:"source_type,omitempty"`
	Purpose       string         `yaml:"purpose,omitempty"`
	Tags          []string       `yaml:"tags,omitempty"`
	Content       string         `yaml:"content,omitempty"`
	ContentPath   string         `yaml:"content_path,omitempty"`
	ContentGlob   string         `yaml:"content_glob,omitempty"`
	NameTemplate  string         `yaml:"name_template,omitempty"`
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

// knowledgeTally accumulates outcomes across both the single-file path and
// the content_glob fan-out path, so one summary line covers a whole run.
type knowledgeTally struct {
	created, updated, noop, skipped int
}

func (t *knowledgeTally) print() {
	fmt.Printf("provision: knowledge summary: created=%d updated=%d noop=%d skipped=%d\n",
		t.created, t.updated, t.noop, t.skipped)
}

// applyKnowledge applies all knowledge files from the knowledge-base/ subdirectory.
func applyKnowledge(c *provisionClient, dir string, orgID uint) error {
	kd := filepath.Join(dir, "knowledge-base")
	if !fileExists(kd) {
		return nil
	}
	return provisionKnowledgeBase(c, kd, orgID)
}

// provisionKnowledgeBase walks every *.yaml file under dir. Entries with
// content_glob fan out into many knowledge files (see provisionKnowledgeGlob);
// all other entries follow the original single-file content/content_path
// path. One tally is printed at the end covering both.
func provisionKnowledgeBase(c *provisionClient, dir string, orgID uint) error {
	entries, err := walkKnowledgeYAMLs(dir)
	if err != nil {
		return fmt.Errorf("knowledge: walk %s: %w", dir, err)
	}
	if len(entries) == 0 {
		fmt.Printf("provision: no knowledge files found under %s\n", dir)
		return nil
	}

	tally := &knowledgeTally{}
	for _, path := range entries {
		cfg, err := readKnowledgeConfig(path)
		if err != nil {
			return err
		}
		if err := validateKnowledgeConfig(path, cfg); err != nil {
			return err
		}

		if cfg.ContentGlob != "" {
			if err := provisionKnowledgeGlob(c, path, cfg, orgID, tally); err != nil {
				return err
			}
			continue
		}

		content, err := resolveKnowledgeContent(path, cfg)
		if err != nil {
			// Skip-not-fail policy: content_path that points at a missing
			// file (typically gitignored JSONs) is a recoverable state.
			if os.IsNotExist(err) || isContentPathMissing(err) {
				c.Warn("knowledge file %s: content file missing on disk — skipping. Run `provision kb-import --from <source-url>` to populate from another Studio.", path)
				tally.skipped++
				continue
			}
			return fmt.Errorf("knowledge file %s: %w", path, err)
		}
		if err := upsertKnowledgeEntry(c, orgID, cfg, content, tally); err != nil {
			return err
		}
	}

	tally.print()
	return nil
}

// readKnowledgeConfig parses one knowledge-base YAML file, returning an
// error instead of exiting so callers (including tests) can handle
// malformed input without a subprocess.
func readKnowledgeConfig(path string) (knowledgeFileConfig, error) {
	var cfg knowledgeFileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yamlUnmarshalStrict(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// validateKnowledgeConfig enforces the mutual-exclusion rules for one
// knowledge-base YAML: exactly one of content/content_path/content_glob,
// and content_glob entries never hand-set name (it's derived per match).
func validateKnowledgeConfig(path string, cfg knowledgeFileConfig) error {
	hasGlob := cfg.ContentGlob != ""
	hasInline := cfg.Content != ""
	hasPath := cfg.ContentPath != ""

	if hasGlob {
		if hasInline || hasPath {
			return fmt.Errorf("%s: content_glob cannot be combined with content or content_path", path)
		}
		if strings.TrimSpace(cfg.Name) != "" {
			return fmt.Errorf("%s: content_glob generates names per matched file — remove the top-level `name` field (use name_template to customize)", path)
		}
		return nil
	}

	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("%s: name is required", path)
	}
	if hasInline && hasPath {
		return fmt.Errorf("%s: set either `content` or `content_path`, not both", path)
	}
	if !hasInline && !hasPath {
		return fmt.Errorf("%s: set either `content` (inline) or `content_path` (file on disk)", path)
	}
	return nil
}

// upsertKnowledgeEntry POSTs one knowledge file upsert and records the
// resulting action (created/updated/noop) on tally. Shared by the
// single-file path and the content_glob fan-out path.
func upsertKnowledgeEntry(c *provisionClient, orgID uint, cfg knowledgeFileConfig, content string, tally *knowledgeTally) error {
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
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("knowledge upsert %q: marshal payload: %w", cfg.Name, err)
	}

	body, status, err := c.write("POST", "/admin/knowledge-files/upsert", payloadBytes)
	if err != nil || status >= 300 {
		return fmt.Errorf("knowledge upsert %q: status=%d err=%v body=%s",
			cfg.Name, status, err, provisionSummarize(body))
	}
	if c.dryRun {
		fmt.Printf("[dry-run] knowledge upsert %q (file_type=%q)\n", cfg.Name, cfg.FileType)
		return nil
	}
	var resp knowledgeUpsertResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("knowledge upsert %q: parse response: %w (body=%s)", cfg.Name, err, provisionSummarize(body))
	}
	switch resp.Action {
	case "created":
		tally.created++
		fmt.Printf("CREATE knowledge %q id=%d file_type=%q\n", resp.Name, resp.ID, resp.FileType)
	case "updated":
		tally.updated++
		fmt.Printf("UPDATE knowledge %q id=%d file_type=%q\n", resp.Name, resp.ID, resp.FileType)
	case "noop":
		tally.noop++
		fmt.Printf("NOOP   knowledge %q id=%d file_type=%q (content unchanged)\n", resp.Name, resp.ID, resp.FileType)
	default:
		fmt.Printf("?      knowledge %q action=%q\n", resp.Name, resp.Action)
	}
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

// resolveKnowledgeContent returns the content bytes for a single-file entry.
// Validation (exactly one of content/content_path) already happened in
// validateKnowledgeConfig by the time this is called.
func resolveKnowledgeContent(yamlPath string, cfg knowledgeFileConfig) (string, error) {
	if cfg.Content != "" {
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

// resolveKnowledgeName expands a name_template against one glob match.
// relNoExt is the match's path relative to the glob's literal base
// directory, forward-slash-separated, extension already stripped.
//
// Placeholders:
//
//	{{relpath}} - relNoExt itself (the default template when tmpl == "")
//	{{stem}}    - final path segment (e.g. "DEVELOPER" for "delivery/DEVELOPER")
//	{{dir}}     - immediate parent directory name ("" if relNoExt has no parent)
func resolveKnowledgeName(tmpl, relNoExt string) string {
	if tmpl == "" {
		tmpl = "{{relpath}}"
	}
	stem := path.Base(relNoExt)
	dir := path.Dir(relNoExt)
	if dir == "." {
		dir = ""
	} else {
		dir = path.Base(dir)
	}
	r := strings.NewReplacer(
		"{{relpath}}", relNoExt,
		"{{stem}}", stem,
		"{{dir}}", dir,
	)
	return r.Replace(tmpl)
}

// globMatch pairs one matched file with its derived knowledge-file name.
type globMatch struct {
	path string
	name string
}

// provisionKnowledgeGlob expands cfg.ContentGlob relative to the YAML's own
// directory and upserts one knowledge file per match. Shared fields
// (file_type, source_type, purpose, tags, metadata, change_summary) apply
// to every generated entry; name is derived per match via cfg.NameTemplate
// (default "{{relpath}}"). Supports "../" traversal so a glob can reach a
// sibling directory or sibling repo checkout, and "**" for recursive
// matching (doublestar.FilepathGlob operates on real OS paths, not an
// fs.FS root, so ".." segments are not restricted the way they would be
// under doublestar.Glob(os.DirFS(...), ...)).
//
// Names are resolved for every match up front, before any upsert runs, so a
// name_template that collides two matches onto the same name (or resolves
// to an empty name) fails fast instead of silently overwriting one match's
// content with another's partway through the run.
func provisionKnowledgeGlob(c *provisionClient, yamlPath string, cfg knowledgeFileConfig, orgID uint, tally *knowledgeTally) error {
	pattern := cfg.ContentGlob
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(filepath.Dir(yamlPath), pattern)
	}
	slashPattern := filepath.ToSlash(pattern)
	base, _ := doublestar.SplitPattern(slashPattern)

	// WithFilesOnly: a bare "**" (or any pattern without an extension
	// filter) also matches directories. Without this, a matched directory
	// reaches os.ReadFile below and fails with "is a directory" after
	// alphabetically-earlier matches have already been upserted.
	matches, err := doublestar.FilepathGlob(pattern, doublestar.WithFilesOnly())
	if err != nil {
		return fmt.Errorf("content_glob %q: %w", cfg.ContentGlob, err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		c.Warn("knowledge file %s: content_glob %q matched no files — skipping", yamlPath, cfg.ContentGlob)
		return nil
	}

	entries := make([]globMatch, 0, len(matches))
	seen := make(map[string]string, len(matches)) // derived name -> first match that produced it
	for _, m := range matches {
		rel, err := filepath.Rel(base, m)
		if err != nil {
			return fmt.Errorf("content_glob %q: relative path for %s: %w", cfg.ContentGlob, m, err)
		}
		rel = filepath.ToSlash(rel)
		relNoExt := strings.TrimSuffix(rel, filepath.Ext(rel))
		name := resolveKnowledgeName(cfg.NameTemplate, relNoExt)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("content_glob %q: name_template %q resolved to an empty name for %s — adjust the template", cfg.ContentGlob, cfg.NameTemplate, m)
		}
		if prior, ok := seen[name]; ok {
			return fmt.Errorf("content_glob %q: name_template %q produces the same name %q for both %s and %s — adjust the template so matches stay distinct", cfg.ContentGlob, cfg.NameTemplate, name, prior, m)
		}
		seen[name] = m
		entries = append(entries, globMatch{path: m, name: name})
	}

	for _, e := range entries {
		data, err := os.ReadFile(e.path)
		if err != nil {
			return fmt.Errorf("content_glob %q: read %s: %w", cfg.ContentGlob, e.path, err)
		}

		entry := cfg
		entry.Name = e.name
		if err := upsertKnowledgeEntry(c, orgID, entry, string(data), tally); err != nil {
			return err
		}
	}
	return nil
}
