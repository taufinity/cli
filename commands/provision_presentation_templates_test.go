package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// presentationTemplatesTestServer fakes GET/POST/PUT /api/presentation-templates
// so applyPresentationTemplates / pullProvisionPresentationTemplates can be
// exercised without a real Studio instance.
type presentationTemplatesTestServer struct {
	mu        sync.Mutex
	templates []provisionPresentationTemplateDef
	creates   []provisionPresentationTemplateDef
	updates   []presentationTemplateUpdateCall
	nextID    uint
}

type presentationTemplateUpdateCall struct {
	UUID string
	Def  provisionPresentationTemplateDef
}

func (s *presentationTemplatesTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/presentation-templates":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.templates)

		case r.Method == http.MethodPost && r.URL.Path == "/api/presentation-templates":
			var body provisionPresentationTemplateDef
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.nextID++
			body.ID = s.nextID
			body.UUID = "prtp_test" + strconv.Itoa(int(s.nextID))
			s.templates = append(s.templates, body)
			s.creates = append(s.creates, body)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(body)

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/presentation-templates/"):
			uuid := strings.TrimPrefix(r.URL.Path, "/api/presentation-templates/")
			var body provisionPresentationTemplateDef
			_ = json.NewDecoder(r.Body).Decode(&body)
			body.UUID = uuid
			s.updates = append(s.updates, presentationTemplateUpdateCall{UUID: uuid, Def: body})
			for i, t := range s.templates {
				if t.UUID == uuid {
					if body.Name != "" {
						s.templates[i].Name = body.Name
					}
					s.templates[i].CompiledTemplate = body.CompiledTemplate
					s.templates[i].IsDefault = body.IsDefault
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(body)

		default:
			http.NotFound(w, r)
		}
	})
}

// --- parsePresentationTemplateFile / renderPresentationTemplateFile ---

func TestParsePresentationTemplateFile_WithHeader(t *testing.T) {
	raw := []byte("<!-- taufinity-provision\n" +
		"name: Taufinity Branded\n" +
		"uuid: prtp_dac28b77\n" +
		"is_default: true\n" +
		"branch: main\n" +
		"-->\n" +
		"<html>body</html>\n")

	meta, content := parsePresentationTemplateFile(raw)
	if meta.Name != "Taufinity Branded" || meta.UUID != "prtp_dac28b77" || !meta.IsDefault || meta.Branch != "main" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if content != "<html>body</html>\n" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestParsePresentationTemplateFile_NoHeader(t *testing.T) {
	raw := []byte("<html>hand authored</html>\n")
	meta, content := parsePresentationTemplateFile(raw)
	if meta != (presentationTemplateMeta{}) {
		t.Fatalf("expected zero-value meta, got %+v", meta)
	}
	if content != string(raw) {
		t.Fatalf("expected content unchanged, got %q", content)
	}
}

func TestRenderPresentationTemplateFile_RoundTrip(t *testing.T) {
	meta := presentationTemplateMeta{Name: "Taufinity Branded", UUID: "prtp_dac28b77", IsDefault: true, Branch: "main"}
	content := "<html>body</html>\n"
	raw := renderPresentationTemplateFile(meta, content)

	gotMeta, gotContent := parsePresentationTemplateFile(raw)
	if gotMeta != meta {
		t.Fatalf("round-trip meta mismatch: got %+v, want %+v", gotMeta, meta)
	}
	if gotContent != content {
		t.Fatalf("round-trip content mismatch: got %q, want %q", gotContent, content)
	}
}

// --- pinPresentationTemplateUUID ---

func TestPinPresentationTemplateUUID_InsertsAfterName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.html")
	raw := "<!-- taufinity-provision\nname: New Template\nis_default: false\n-->\n<html></html>\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := pinPresentationTemplateUUID(path, "", "prtp_abc123"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	got, _ := os.ReadFile(path)
	meta, _ := parsePresentationTemplateFile(got)
	if meta.UUID != "prtp_abc123" {
		t.Fatalf("expected uuid pinned, got meta=%+v raw=%q", meta, string(got))
	}
}

func TestPinPresentationTemplateUUID_ReplacesStalePin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.html")
	raw := "<!-- taufinity-provision\nname: T\nuuid: prtp_old\nis_default: false\n-->\n<html></html>\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := pinPresentationTemplateUUID(path, "prtp_old", "prtp_new"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	got, _ := os.ReadFile(path)
	meta, _ := parsePresentationTemplateFile(got)
	if meta.UUID != "prtp_new" {
		t.Fatalf("expected uuid updated to prtp_new, got %+v", meta)
	}
}

func TestPinPresentationTemplateUUID_NoopWhenMatching(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.html")
	raw := "<!-- taufinity-provision\nname: T\nuuid: prtp_same\nis_default: false\n-->\n<html></html>\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := os.ReadFile(path)

	if err := pinPresentationTemplateUUID(path, "prtp_same", "prtp_same"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("expected file unchanged when uuid already matches")
	}
}

// --- applyPresentationTemplates ---

func TestApplyPresentationTemplates_CreatesNewFile(t *testing.T) {
	srv := &presentationTemplatesTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	_ = os.MkdirAll(td, 0o755)
	raw := renderPresentationTemplateFile(
		presentationTemplateMeta{Name: "Brand New", IsDefault: false, Branch: "main"},
		"<html>new</html>\n",
	)
	path := filepath.Join(td, "brand-new.html")
	_ = os.WriteFile(path, raw, 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Fatalf("applyPresentationTemplates: %v", err)
	}

	if len(srv.creates) != 1 {
		t.Fatalf("expected 1 create, got %d: %+v", len(srv.creates), srv.creates)
	}
	if srv.creates[0].Name != "Brand New" {
		t.Errorf("unexpected created name: %q", srv.creates[0].Name)
	}

	// The file on disk should now have the server-assigned uuid pinned back in.
	got, _ := os.ReadFile(path)
	meta, _ := parsePresentationTemplateFile(got)
	if meta.UUID == "" {
		t.Errorf("expected uuid pinned into file after create, got empty")
	}
}

func TestApplyPresentationTemplates_UpdatesOnContentChange(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_existing", Name: "Existing", IsDefault: true, CompiledTemplate: "<html>old</html>\n"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	_ = os.MkdirAll(td, 0o755)
	raw := renderPresentationTemplateFile(
		presentationTemplateMeta{Name: "Existing", UUID: "prtp_existing", IsDefault: true, Branch: "main"},
		"<html>new</html>\n",
	)
	_ = os.WriteFile(filepath.Join(td, "existing.html"), raw, 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Fatalf("applyPresentationTemplates: %v", err)
	}

	if len(srv.updates) != 1 {
		t.Fatalf("expected 1 update, got %d: %+v", len(srv.updates), srv.updates)
	}
	if srv.updates[0].UUID != "prtp_existing" {
		t.Errorf("unexpected update target: %q", srv.updates[0].UUID)
	}
	if len(srv.creates) != 0 {
		t.Errorf("expected zero creates, got %d", len(srv.creates))
	}
}

func TestApplyPresentationTemplates_NoopWhenUnchanged(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_existing", Name: "Existing", IsDefault: true, CompiledTemplate: "<html>same</html>\n"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	_ = os.MkdirAll(td, 0o755)
	raw := renderPresentationTemplateFile(
		presentationTemplateMeta{Name: "Existing", UUID: "prtp_existing", IsDefault: true, Branch: "main"},
		"<html>same</html>\n",
	)
	_ = os.WriteFile(filepath.Join(td, "existing.html"), raw, 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Fatalf("applyPresentationTemplates: %v", err)
	}

	if len(srv.updates) != 0 || len(srv.creates) != 0 {
		t.Fatalf("expected NOOP (no writes), got updates=%+v creates=%+v", srv.updates, srv.creates)
	}
}

func TestApplyPresentationTemplates_ErrorsWhenPinnedUUIDMissing(t *testing.T) {
	srv := &presentationTemplatesTestServer{} // no templates on server
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	_ = os.MkdirAll(td, 0o755)
	raw := renderPresentationTemplateFile(
		presentationTemplateMeta{Name: "Ghost", UUID: "prtp_deleted", IsDefault: false, Branch: "main"},
		"<html>ghost</html>\n",
	)
	_ = os.WriteFile(filepath.Join(td, "ghost.html"), raw, 0o644)

	c := newProvisionClient(ts.URL, "test-key", false)
	err := applyPresentationTemplates(c, dir, 12)
	if err == nil {
		t.Fatal("expected error for pinned uuid not found on server, got nil")
	}
	if !strings.Contains(err.Error(), "prtp_deleted") {
		t.Errorf("expected error to mention the missing uuid, got: %v", err)
	}
}

func TestApplyPresentationTemplates_MissingDirIsNoop(t *testing.T) {
	srv := &presentationTemplatesTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir() // no presentation-templates/ subdir
	c := newProvisionClient(ts.URL, "test-key", false)
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Errorf("missing dir should be no-op, got error: %v", err)
	}
}

// --- pullProvisionPresentationTemplates ---

func TestPullProvisionPresentationTemplates_WritesFiles(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_a", Name: "Taufinity Branded", IsDefault: true, Branch: "main", CompiledTemplate: "<html>a</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("pull: %v", err)
	}

	path := filepath.Join(td, "taufinity-branded.html")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected pulled file at %s: %v", path, err)
	}
	meta, content := parsePresentationTemplateFile(raw)
	if meta.Name != "Taufinity Branded" || meta.UUID != "prtp_a" || !meta.IsDefault {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if content != "<html>a</html>" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestPullProvisionPresentationTemplates_DryRunWritesNothing(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_a", Name: "Taufinity Branded", IsDefault: true, CompiledTemplate: "<html>a</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, true); err != nil {
		t.Fatalf("pull dryRun: %v", err)
	}
	if fileExists(td) {
		t.Errorf("dry-run should not create the directory or any files")
	}
}

func TestPullProvisionPresentationTemplates_DedupesSlugCollision(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_a", Name: "Same Name", CompiledTemplate: "<html>a</html>"},
			{UUID: "prtp_b", Name: "Same Name", CompiledTemplate: "<html>b</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("pull: %v", err)
	}

	entries, _ := filepath.Glob(filepath.Join(td, "*.html"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file written (second collides), got %d: %v", len(entries), entries)
	}
}

// --- pull -> apply round trip ---

func TestPresentationTemplates_PullThenApplyIsNoop(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_rt", Name: "Round Trip", IsDefault: true, Branch: "main", CompiledTemplate: "<html>content</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(srv.updates) != 0 || len(srv.creates) != 0 {
		t.Fatalf("expected pull->apply round trip to be a NOOP, got updates=%+v creates=%+v", srv.updates, srv.creates)
	}
}

// TestPullProvisionPresentationTemplates_RemovesStaleFileOnRename — a
// server-side rename produces a new slug (new filename) while keeping the
// same uuid. Without cleanup, the old file survives as an orphan that
// apply's *.html glob still matches by uuid, silently reverting the rename
// on next apply. Pull must remove it.
func TestPullProvisionPresentationTemplates_RemovesStaleFileOnRename(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_a", Name: "Foo", CompiledTemplate: "<html>v1</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	if !fileExists(filepath.Join(td, "foo.html")) {
		t.Fatalf("expected foo.html after first pull")
	}

	// Rename server-side: same uuid, new name.
	srv.mu.Lock()
	srv.templates[0].Name = "Bar"
	srv.mu.Unlock()

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("second pull: %v", err)
	}

	if fileExists(filepath.Join(td, "foo.html")) {
		t.Errorf("expected stale foo.html to be removed after rename, but it still exists")
	}
	if !fileExists(filepath.Join(td, "bar.html")) {
		t.Errorf("expected bar.html to exist after rename")
	}

	entries, _ := filepath.Glob(filepath.Join(td, "*.html"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file after rename cleanup, got %d: %v", len(entries), entries)
	}

	// The whole point: apply must now see NOOP, not silently revert the rename.
	if err := applyPresentationTemplates(c, dir, 12); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(srv.updates) != 0 || len(srv.creates) != 0 {
		t.Fatalf("expected apply after rename cleanup to be a NOOP, got updates=%+v creates=%+v", srv.updates, srv.creates)
	}
}

// TestPullProvisionPresentationTemplates_DryRunDoesNotRemoveStaleFile —
// dry-run must report what it would remove without touching disk.
func TestPullProvisionPresentationTemplates_DryRunDoesNotRemoveStaleFile(t *testing.T) {
	srv := &presentationTemplatesTestServer{
		templates: []provisionPresentationTemplateDef{
			{UUID: "prtp_a", Name: "Foo", CompiledTemplate: "<html>v1</html>"},
		},
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	td := filepath.Join(dir, "presentation-templates")
	c := newProvisionClient(ts.URL, "test-key", false)

	if err := pullProvisionPresentationTemplates(c, 12, td, false); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	srv.mu.Lock()
	srv.templates[0].Name = "Bar"
	srv.mu.Unlock()

	if err := pullProvisionPresentationTemplates(c, 12, td, true); err != nil {
		t.Fatalf("dry-run pull: %v", err)
	}

	if !fileExists(filepath.Join(td, "foo.html")) {
		t.Errorf("dry-run must not remove the stale file")
	}
	if fileExists(filepath.Join(td, "bar.html")) {
		t.Errorf("dry-run must not write the new file")
	}
}

// TestRenderPresentationTemplateFile_SanitizesEmbeddedNewlineInName — a
// crafted Name containing "\n-->\n" would otherwise let a rewritten file's
// header close early, spilling attacker-controlled content (including a
// forged uuid: line) into what apply treats as pure compiled_template HTML.
func TestRenderPresentationTemplateFile_SanitizesEmbeddedNewlineInName(t *testing.T) {
	evilName := "Evil\n-->\n<script>alert(1)</script>\nuuid: prtp_forged"
	meta := presentationTemplateMeta{Name: evilName, UUID: "prtp_real", IsDefault: false, Branch: "main"}
	content := "<html>real content</html>"

	raw := renderPresentationTemplateFile(meta, content)

	gotMeta, gotContent := parsePresentationTemplateFile(raw)
	if gotMeta.UUID != "prtp_real" {
		t.Fatalf("expected uuid to remain prtp_real, got %q (header was corrupted)", gotMeta.UUID)
	}
	if gotContent != content {
		t.Fatalf("expected content to remain untouched, got %q", gotContent)
	}
	if strings.Contains(gotMeta.Name, "\n") {
		t.Errorf("expected name to have no embedded newline, got %q", gotMeta.Name)
	}
}
