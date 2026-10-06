package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// generalSettingsServer fakes the general-settings and category-pages
// endpoints of site 6. GET returns stored; every PUT is recorded. When
// storeOnPut is true a general PUT merges its keys into stored, the way
// UpdateGeneralSettings does for the keys it knows; keys in ignore are dropped,
// the way a Studio without those keys in its allowlist does.
type generalSettingsFake struct {
	mu         sync.Mutex
	stored     map[string]any
	ignore     map[string]bool
	gets       int
	puts       []sitePutCall
	storeOnPut bool
}

func (f *generalSettingsFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sites/6/settings/general":
			f.gets++
			_ = json.NewEncoder(w).Encode(f.stored)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/sites/6/settings/"):
			b, _ := io.ReadAll(r.Body)
			f.puts = append(f.puts, sitePutCall{Path: r.URL.Path, Body: b})
			if f.storeOnPut && r.URL.Path == "/api/sites/6/settings/general" {
				var req map[string]any
				_ = json.Unmarshal(b, &req)
				for k, v := range req {
					if !f.ignore[k] {
						f.stored[k] = v
					}
				}
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (f *generalSettingsFake) generalPuts() []sitePutCall {
	var out []sitePutCall
	for _, p := range f.puts {
		if p.Path == "/api/sites/6/settings/general" {
			out = append(out, p)
		}
	}
	return out
}

// What production site 6 holds on 2026-10-06.
func prodSite6General() map[string]any {
	return map[string]any{
		"name":   "VoorPositiviteit",
		"domain": "www.voorpositiviteit.nl",
		"category_index_page": map[string]any{
			"path":     "spreuken",
			"template": "spreuken-index.html",
		},
	}
}

const vpCategoryIndexYAML = `id: 6
category_index_page:
  path: spreuken
  template: spreuken-index.html
  article_meta_fields:
    likes: "QuoteData.likes"
    book_quote: "book_quote"
`

// --- parsing --------------------------------------------------------------

func TestSiteYAMLGeneralKeysAbsentStayNil(t *testing.T) {
	var sy siteYAML
	if err := yamlUnmarshalStrict([]byte("id: 6\nname: X\n"), &sy); err != nil {
		t.Fatal(err)
	}
	if sy.CategoryIndexPage != nil || sy.Redirects != nil || sy.InfraPagesUnderPrefix != nil {
		t.Fatalf("absent keys must decode as nil, got %+v", sy)
	}
	if p := siteGeneralPayload(sy); p != nil {
		t.Fatalf("no general keys set: payload must be nil, got %v", p)
	}
}

func TestSiteYAMLGeneralKeysNullIsAbsent(t *testing.T) {
	var sy siteYAML
	err := yamlUnmarshalStrict([]byte("id: 6\ncategory_index_page:\nredirects:\ninfra_pages_under_prefix:\n"), &sy)
	if err != nil {
		t.Fatal(err)
	}
	if p := siteGeneralPayload(sy); p != nil {
		t.Fatalf("null values must be treated as absent, got %v", p)
	}
}

func TestSiteYAMLGeneralKeysExplicitEmptyIsSent(t *testing.T) {
	var sy siteYAML
	err := yamlUnmarshalStrict([]byte("id: 6\ncategory_index_page: {}\nredirects: []\ninfra_pages_under_prefix: false\n"), &sy)
	if err != nil {
		t.Fatal(err)
	}
	p := siteGeneralPayload(sy)
	b, _ := json.Marshal(p)
	want := `{"category_index_page":{},"infra_pages_under_prefix":false,"redirects":[]}`
	if string(b) != want {
		t.Fatalf("payload = %s, want %s", b, want)
	}
}

func TestSiteYAMLParsesRedirectsAndInfraPages(t *testing.T) {
	var sy siteYAML
	src := `id: 6
infra_pages_under_prefix: true
redirects:
  - path: /spreuken/thema/oud/
    target: /spreuken/thema/nieuw/
  - path: /spreuken/x.html
    target: https://www.voorpositiviteit.nl/spreuken/
    title: Spreuken
`
	if err := yamlUnmarshalStrict([]byte(src), &sy); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(siteGeneralPayload(sy))
	want := `{"infra_pages_under_prefix":true,"redirects":[{"path":"/spreuken/thema/oud/","target":"/spreuken/thema/nieuw/"},{"path":"/spreuken/x.html","target":"https://www.voorpositiviteit.nl/spreuken/","title":"Spreuken"}]}`
	if string(b) != want {
		t.Fatalf("payload = %s\nwant      %s", b, want)
	}
}

func TestSiteYAMLRejectsUnknownRedirectKey(t *testing.T) {
	var sy siteYAML
	err := yamlUnmarshalStrict([]byte("id: 6\nredirects:\n  - path: /a/\n    targt: /b/\n"), &sy)
	if err == nil {
		t.Fatal("a misspelled redirect key must fail the strict decoder")
	}
}

func TestSiteYAMLRejectsNonBoolInfraPages(t *testing.T) {
	var sy siteYAML
	if err := yamlUnmarshalStrict([]byte("id: 6\ninfra_pages_under_prefix: yes please\n"), &sy); err == nil {
		t.Fatal("a non-bool infra_pages_under_prefix must fail to parse")
	}
}

func TestCategoryPagesExcludeValuesValidation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		ok         bool
	}{
		{"absent", "category_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n", true},
		{"list", "category_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n    exclude_values: [geen_specifieke_situatie]\n", true},
		{"empty list", "category_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n    exclude_values: []\n", true},
		{"string", "category_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n    exclude_values: geen\n", false},
		{"list of maps", "category_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n    exclude_values: [{a: b}]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sy siteYAML
			if err := yamlUnmarshalStrict([]byte("id: 6\n"+tc.yaml), &sy); err != nil {
				t.Fatal(err)
			}
			err := validateCategoryPages(sy.CategoryPages)
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "exclude_values")) {
				t.Fatalf("want an exclude_values error, got %v", err)
			}
		})
	}
}

// --- apply ----------------------------------------------------------------

func TestCategoryPagesPushCarriesExcludeValues(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\ncategory_pages:\n  - meta_field: QuoteData.situation\n    url_pattern: spreuken/situatie/{slug}\n    exclude_values: [geen_specifieke_situatie]\n"})

	if err := applySiteDir(c, dir, 3, false); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || f.puts[0].Path != "/api/sites/6/settings/category-pages" {
		t.Fatalf("want one category-pages PUT, got %+v", f.puts)
	}
	if !strings.Contains(string(f.puts[0].Body), `"exclude_values":["geen_specifieke_situatie"]`) {
		t.Fatalf("category-pages body lost exclude_values: %s", f.puts[0].Body)
	}
}

func TestCategoryPagesRefusesBadExcludeValuesBeforeAnyWrite(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\ncategory_index_page: {path: spreuken}\ncategory_pages:\n  - meta_field: a\n    url_pattern: x/{slug}\n    exclude_values: geen\n"})

	if err := applySiteDir(c, dir, 3, false); err == nil {
		t.Fatal("want an error for a string exclude_values")
	}
	if len(f.puts) != 0 {
		t.Fatalf("nothing may be written, got %+v", f.puts)
	}
}

func TestAbsentGeneralKeysAreNotSent(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": vpSiteYAML})

	if err := applySiteDir(c, dir, 3, false); err != nil {
		t.Fatal(err)
	}
	if got := f.generalPuts(); len(got) != 0 {
		t.Fatalf("site.yaml sets no general key: no general PUT allowed, got %+v", got)
	}
	if f.gets != 0 {
		t.Fatalf("no general key set: no GET needed, got %d", f.gets)
	}
}

func TestGeneralKeysPutSendsOnlyTheSetKeys(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General(), storeOnPut: true}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": vpCategoryIndexYAML})

	if err := applySiteDir(c, dir, 3, false); err != nil {
		t.Fatal(err)
	}
	puts := f.generalPuts()
	if len(puts) != 1 {
		t.Fatalf("want one general PUT, got %+v", f.puts)
	}
	var body map[string]any
	if err := json.Unmarshal(puts[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["category_index_page"] == nil {
		t.Fatalf("PUT must carry only category_index_page, got %s", puts[0].Body)
	}
	want := `{"article_meta_fields":{"book_quote":"book_quote","likes":"QuoteData.likes"},"path":"spreuken","template":"spreuken-index.html"}`
	got, _ := json.Marshal(body["category_index_page"])
	if string(got) != want {
		t.Fatalf("category_index_page = %s\nwant                  %s", got, want)
	}
}

func TestGeneralKeysDiffShowsCategoryIndexPageChange(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", true) // provision diff
	dir := writeSiteDir(t, map[string]string{"site.yaml": vpCategoryIndexYAML})

	out := captureStdout(t, func() {
		if err := applySiteDir(c, dir, 3, false); err != nil {
			t.Errorf("applySiteDir: %v", err)
		}
	})
	for _, want := range []string{
		"UPDATE general settings site=6 (from site.yaml: category_index_page)",
		"category_index_page.article_meta_fields  (absent) -> {\"book_quote\":\"book_quote\",\"likes\":\"QuoteData.likes\"}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "category_index_page.path") || strings.Contains(out, "category_index_page.template") {
		t.Errorf("unchanged path/template must not be listed\n---\n%s", out)
	}
	if f.gets != 1 {
		t.Fatalf("diff must read live settings once, got %d GETs", f.gets)
	}
	if len(f.puts) != 0 {
		t.Fatalf("diff must not write, got %+v", f.puts)
	}
}

func TestGeneralKeysDiffShowsClearOfExplicitEmpty(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", true)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\ncategory_index_page: {}\n"})

	out := captureStdout(t, func() {
		if err := applySiteDir(c, dir, 3, false); err != nil {
			t.Errorf("applySiteDir: %v", err)
		}
	})
	for _, want := range []string{
		"category_index_page.path  spreuken -> (absent)",
		"category_index_page.template  spreuken-index.html -> (absent)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q\n---\n%s", want, out)
		}
	}
}

func TestGeneralKeysNoopWhenLiveMatches(t *testing.T) {
	stored := prodSite6General()
	stored["infra_pages_under_prefix"] = true
	f := &generalSettingsFake{stored: stored}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\ninfra_pages_under_prefix: true\ncategory_index_page:\n  path: spreuken\n  template: spreuken-index.html\n"})

	out := captureStdout(t, func() {
		if err := applySiteDir(c, dir, 3, false); err != nil {
			t.Errorf("applySiteDir: %v", err)
		}
	})
	if len(f.generalPuts()) != 0 {
		t.Fatalf("live matches site.yaml: no PUT, got %+v", f.puts)
	}
	if !strings.Contains(out, "NOOP general settings site=6") {
		t.Errorf("want a NOOP line\n---\n%s", out)
	}
}

func TestGeneralKeysDiffShowsNewKeysAsAbsentRemotely(t *testing.T) {
	f := &generalSettingsFake{stored: prodSite6General()}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", true)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\ninfra_pages_under_prefix: true\nredirects:\n  - path: /a/\n    target: /b/\n"})

	out := captureStdout(t, func() {
		if err := applySiteDir(c, dir, 3, false); err != nil {
			t.Errorf("applySiteDir: %v", err)
		}
	})
	for _, want := range []string{
		"infra_pages_under_prefix  (absent) -> true",
		`redirects  (absent) -> [{"path":"/a/","target":"/b/"}]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q\n---\n%s", want, out)
		}
	}
}

func TestGeneralKeysWarnWhenStudioDropsAKey(t *testing.T) {
	f := &generalSettingsFake{
		stored:     prodSite6General(),
		storeOnPut: true,
		ignore:     map[string]bool{"redirects": true}, // a Studio without the key in its allowlist
	}
	ts := f.server(t)
	c := newProvisionClient(ts.URL, "k", false)
	dir := writeSiteDir(t, map[string]string{"site.yaml": "id: 6\nredirects:\n  - path: /a/\n    target: /b/\n"})

	_ = captureStdout(t, func() {
		if err := applySiteDir(c, dir, 3, false); err != nil {
			t.Errorf("applySiteDir: %v", err)
		}
	})
	if c.WarningCount() != 1 || !strings.Contains(c.warnings[0], "redirects") {
		t.Fatalf("want one warning naming redirects, got %v", c.warnings)
	}
}
