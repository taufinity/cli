// provision_presentation_templates.go — push (apply) and shared list/parse
// logic for Studio presentation templates (compiled_template HTML, used by
// the presentations engine's reveal.js-based renderer).
//
// Unlike dashboards (JSON) or playbooks (YAML), a presentation template is a
// single blob of HTML with embedded Go template directives ({{range .Bullets}},
// {{escapeHTML .}}, ...) — there's no natural place to hang YAML/JSON
// metadata without corrupting the HTML. Metadata (name, uuid pin, is_default,
// branch) instead lives in an HTML comment header at the top of the file,
// mirroring how providers pin a numeric id inside YAML (provision_provider.go
// pinProviderID) — same idea, different host syntax. The filename itself is
// just a slugified copy of the name, for a stable, greppable path; the header
// is always the source of truth.
//
// The list endpoint (GET /presentation-templates) returns compiled_template
// inline and there is no GET-by-uuid, so apply's NOOP check compares directly
// against the list response — no extra detail fetch needed (unlike
// dashboards, whose list response is trimmed).
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// presentationTemplateMetaHeader matches the HTML comment metadata block at
// the top of a presentation-templates/*.html file:
//
//	<!-- taufinity-provision
//	name: Taufinity Branded
//	uuid: prtp_dac28b77
//	is_default: true
//	branch: main
//	-->
var presentationTemplateMetaHeader = regexp.MustCompile(`(?s)^<!-- taufinity-provision\n(.*?)\n-->\n?`)

// presentationTemplateMeta is the parsed metadata header. Name is required
// (apply refuses a file with no name, in the header or derivable otherwise);
// the rest are optional and default the same way the server does.
type presentationTemplateMeta struct {
	Name      string
	UUID      string
	IsDefault bool
	Branch    string
}

// provisionPresentationTemplateDef is the wire shape from GET /presentation-templates.
type provisionPresentationTemplateDef struct {
	ID               uint   `json:"id"`
	UUID             string `json:"uuid"`
	Name             string `json:"name"`
	Branch           string `json:"branch,omitempty"`
	IsDefault        bool   `json:"is_default"`
	CompiledTemplate string `json:"compiled_template"`
}

// listPresentationTemplates fetches every presentation template visible to the org.
func listPresentationTemplates(c *provisionClient, orgID uint) ([]provisionPresentationTemplateDef, error) {
	body, status, err := c.getForOrg("/presentation-templates", orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr("list presentation templates", status, body, err)
	}
	var defs []provisionPresentationTemplateDef
	if err := json.Unmarshal(body, &defs); err != nil {
		return nil, fmt.Errorf("parse presentation templates: %w (body=%s)", err, provisionSummarize(body))
	}
	return defs, nil
}

// parsePresentationTemplateFile splits a local file into its metadata header
// and the raw compiled_template HTML that follows it. A file with no
// recognized header (hand-authored, never pulled) returns a zero-value meta
// — apply then requires the filename itself to double as the name.
func parsePresentationTemplateFile(raw []byte) (presentationTemplateMeta, string) {
	m := presentationTemplateMetaHeader.FindSubmatch(raw)
	if m == nil {
		return presentationTemplateMeta{}, string(raw)
	}
	var meta presentationTemplateMeta
	for _, line := range strings.Split(string(m[1]), "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "name":
			meta.Name = val
		case "uuid":
			meta.UUID = val
		case "is_default":
			meta.IsDefault = val == "true"
		case "branch":
			meta.Branch = val
		}
	}
	content := string(raw[len(m[0]):])
	return meta, content
}

// sanitizeHeaderValue collapses embedded newlines to spaces. name/branch are
// single-line metadata; without this, a value containing "\n-->\n" (however
// unlikely — pulled from a server-side field, not normally hand-typed) would
// prematurely close the taufinity-provision comment, splicing whatever
// follows (including a forged "uuid:" line) into what's meant to be pure
// compiled_template HTML.
func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// renderPresentationTemplateFile assembles the on-disk file: metadata header
// followed by the compiled_template HTML unchanged.
func renderPresentationTemplateFile(meta presentationTemplateMeta, content string) []byte {
	var b strings.Builder
	b.WriteString("<!-- taufinity-provision\n")
	fmt.Fprintf(&b, "name: %s\n", sanitizeHeaderValue(meta.Name))
	if meta.UUID != "" {
		fmt.Fprintf(&b, "uuid: %s\n", sanitizeHeaderValue(meta.UUID))
	}
	fmt.Fprintf(&b, "is_default: %t\n", meta.IsDefault)
	if meta.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", sanitizeHeaderValue(meta.Branch))
	}
	b.WriteString("-->\n")
	b.WriteString(content)
	return []byte(b.String())
}

// pinPresentationTemplateUUID writes `uuid: <liveUUID>` into the file's
// metadata header after a create, or corrects a stale pin. Preserves
// everything else in the file via a targeted line edit, same approach as
// pinProviderID.
func pinPresentationTemplateUUID(path, localUUID, liveUUID string) error {
	if liveUUID == "" || localUUID == liveUUID {
		return nil
	}
	if localUUID != "" {
		fmt.Printf("  WARN: presentation template uuid in file (%s) differs from server (%s) — updating pin in %s\n",
			localUUID, liveUUID, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	uuidLine := regexp.MustCompile(`(?m)^uuid:\s+\S+`)
	newLine := "uuid: " + liveUUID
	if uuidLine.Match(raw) {
		raw = uuidLine.ReplaceAll(raw, []byte(newLine))
		return os.WriteFile(path, raw, 0o644)
	}
	if loc := regexp.MustCompile(`(?m)^name:.*\n`).FindIndex(raw); loc != nil {
		// Header exists (matched by parsePresentationTemplateFile, which
		// requires a "name:" line) but has no uuid line yet — insert one
		// right after the name line.
		insert := []byte(newLine + "\n")
		raw = append(raw[:loc[1]], append(insert, raw[loc[1]:]...)...)
		return os.WriteFile(path, raw, 0o644)
	}
	// No recognized header at all (hand-authored file, no "taufinity-provision"
	// comment) — prepend a minimal one. name is recovered from the filename by
	// the caller before this is reached, so it is not re-derived here.
	raw = append([]byte(fmt.Sprintf("<!-- taufinity-provision\nuuid: %s\n-->\n", liveUUID)), raw...)
	return os.WriteFile(path, raw, 0o644)
}

// applyPresentationTemplates reads all *.html files from
// dir/presentation-templates/ and upserts them via the presentation-templates
// API. Matches by the uuid pinned in the file's metadata header when present,
// else falls back to case-insensitive name matching (adopting an existing
// server-side template and pinning its uuid back into the file). A brand-new
// file (no pin, no name match) is created.
func applyPresentationTemplates(c *provisionClient, dir string, orgID uint) error {
	tmplDir := filepath.Join(dir, "presentation-templates")
	if !fileExists(tmplDir) {
		return nil
	}

	entries, err := filepath.Glob(filepath.Join(tmplDir, "*.html"))
	if err != nil {
		return fmt.Errorf("glob presentation-templates/: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Strings(entries)

	existing, err := listPresentationTemplates(c, orgID)
	if err != nil {
		return err
	}
	byUUID := make(map[string]provisionPresentationTemplateDef, len(existing))
	byName := make(map[string]provisionPresentationTemplateDef, len(existing))
	for _, d := range existing {
		byUUID[d.UUID] = d
		byName[strings.ToLower(d.Name)] = d
	}

	var created, updated, noop int
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		meta, content := parsePresentationTemplateFile(raw)
		name := meta.Name
		if name == "" {
			// Hand-authored file, never pulled — the filename is the only
			// name we have.
			name = strings.TrimSuffix(filepath.Base(path), ".html")
		}

		var cur provisionPresentationTemplateDef
		var found bool
		if meta.UUID != "" {
			cur, found = byUUID[meta.UUID]
			if !found {
				return fmt.Errorf("presentation template %q: uuid %q pinned in file not found on server (deleted? wrong org?)", path, meta.UUID)
			}
		} else if c2, ok := byName[strings.ToLower(name)]; ok {
			cur, found = c2, true
		}

		branch := meta.Branch
		if branch == "" {
			branch = "main"
		}

		if !found {
			payload, _ := json.Marshal(map[string]any{
				"name":              name,
				"branch":            branch,
				"compiled_template": content,
				"is_default":        meta.IsDefault,
			})
			fmt.Printf("CREATE %s\n", name)
			body, status, err := c.writeForOrg("POST", "/presentation-templates", payload, orgID)
			if err != nil || status >= 300 {
				return provisionAPIErr(fmt.Sprintf("create presentation template %q", name), status, body, err)
			}
			var createdDef provisionPresentationTemplateDef
			if err := json.Unmarshal(body, &createdDef); err == nil && createdDef.UUID != "" {
				if err := pinPresentationTemplateUUID(path, meta.UUID, createdDef.UUID); err != nil {
					c.Warn("presentation template %q: created but failed to pin uuid: %v", name, err)
				}
			}
			created++
			continue
		}

		if cur.Name == name && cur.IsDefault == meta.IsDefault && cur.CompiledTemplate == content {
			fmt.Printf("NOOP   %s uuid=%s\n", name, cur.UUID)
			noop++
			continue
		}
		// branch is deliberately omitted here: UpdateTemplateRequest on the
		// server has no branch field at all, so it's create-only — editing
		// branch: in an already-pulled file's header has no effect on apply.
		payload, _ := json.Marshal(map[string]any{
			"name":              name,
			"compiled_template": content,
			"is_default":        meta.IsDefault,
		})
		fmt.Printf("UPDATE %s uuid=%s\n", name, cur.UUID)
		body, status, err := c.writeForOrg("PUT", "/presentation-templates/"+cur.UUID, payload, orgID)
		if err != nil || status >= 300 {
			return provisionAPIErr(fmt.Sprintf("update presentation template %q", name), status, body, err)
		}
		if err := pinPresentationTemplateUUID(path, meta.UUID, cur.UUID); err != nil {
			c.Warn("presentation template %q: updated but failed to pin uuid: %v", name, err)
		}
		updated++
	}

	fmt.Printf("provision: presentation templates summary: created=%d updated=%d noop=%d\n", created, updated, noop)
	return nil
}
