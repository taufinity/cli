package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type sitePutCall struct {
	Path string
	Body json.RawMessage
}

func siteSettingsServer(t *testing.T) (*httptest.Server, *[]sitePutCall) {
	t.Helper()
	var mu sync.Mutex
	calls := []sitePutCall{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/sites/") {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			calls = append(calls, sitePutCall{Path: r.URL.Path, Body: b})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

func writeSiteDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "test2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The shape of voorpositiviteit-templates studio/sites/test2/site.yaml (#70).
const vpSiteYAML = `id: 6
name: VoorPositiviteit
content:
  languages:
    - code: nl
      name: Nederlands
      default: true
    - code: de-DE
      name: Deutsch
ui_translations:
  nl:
    book_order: "Bestel"
    heading_thought: "De gedachte|erachter"
  de-DE:
    book_order: "Bestellen"
    heading_thought: "Der Gedanke|dahinter"
`

func TestSiteYAMLPushesContentAndUITranslations(t *testing.T) {
	ts, calls := siteSettingsServer(t)
	c := newProvisionClient(ts.URL, "test-key", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": vpSiteYAML})

	if err := applySiteDir(c, dir, 3, false); err != nil {
		t.Fatalf("applySiteDir: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("want 2 PUTs, got %d: %+v", len(*calls), *calls)
	}
	content, ui := (*calls)[0], (*calls)[1]
	if content.Path != "/api/sites/6/settings/content" {
		t.Errorf("content path = %s", content.Path)
	}
	var cb struct {
		Languages []siteLanguage `json:"languages"`
	}
	if err := json.Unmarshal(content.Body, &cb); err != nil || len(cb.Languages) != 2 ||
		cb.Languages[0].Code != "nl" || !cb.Languages[0].Default || cb.Languages[1].Code != "de-DE" {
		t.Errorf("content body = %s (err %v)", content.Body, err)
	}
	if strings.Contains(string(content.Body), `"category"`) {
		t.Errorf("content body sends fields site.yaml did not set: %s", content.Body)
	}
	if ui.Path != "/api/sites/6/settings/ui-translations" {
		t.Errorf("ui path = %s", ui.Path)
	}
	var ub map[string]map[string]string
	if err := json.Unmarshal(ui.Body, &ub); err != nil || ub["de-DE"]["heading_thought"] != "Der Gedanke|dahinter" {
		t.Errorf("ui body = %s (err %v)", ui.Body, err)
	}
}

func TestSiteYAMLRefusesAnEmptyTranslation(t *testing.T) {
	ts, calls := siteSettingsServer(t)
	c := newProvisionClient(ts.URL, "test-key", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\nui_translations:\n  nl:\n    book_order: \"\"\n"})

	err := applySiteDir(c, dir, 3, false)
	if err == nil || !strings.Contains(err.Error(), "book_order") {
		t.Fatalf("want an error naming book_order, got %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("nothing may be pushed when validation fails, got %+v", *calls)
	}
}

func TestSiteYAMLRefusesContentInTwoPlaces(t *testing.T) {
	ts, calls := siteSettingsServer(t)
	c := newProvisionClient(ts.URL, "test-key", false)
	dir := writeSiteDir(t, map[string]string{
		"site.yaml":             "id: 6\ncontent:\n  languages:\n    - code: nl\n      name: Nederlands\n",
		"content-settings.yaml": "tone: warm\n",
	})

	err := applySiteDir(c, dir, 3, false)
	if err == nil || !strings.Contains(err.Error(), "both site.yaml and content-settings.yaml") {
		t.Fatalf("want a two-sources error, got %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("nothing may be pushed, got %+v", *calls)
	}
}

func TestSiteYAMLStillRejectsUnknownContentKeys(t *testing.T) {
	var sy siteYAML
	err := yamlUnmarshalStrict([]byte("id: 6\ncontent:\n  langauges: []\n"), &sy)
	if err == nil {
		t.Fatal("a misspelled content key must still fail the strict decoder")
	}
}
