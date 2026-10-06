package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// putPayloadFor runs applyProviders against a fake Studio holding one stored
// provider and returns the decoded PUT payload provision sent for it. It goes
// through mustReadYAML, so it also pins how the real YAML decoder treats an
// absent key versus an explicit empty map.
func putPayloadFor(t *testing.T, providerYAML string) map[string]interface{} {
	t.Helper()
	var put map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/custom-ai-providers":
			json.NewEncoder(w).Encode([]map[string]interface{}{{
				"id":                9,
				"name":              "Quote metadata",
				"slug":              "quote-metadata",
				"response_mappings": `{"quote":"$.quote"}`,
				"request_headers":   `{"X-Old":"1"}`,
			}})
		case r.Method == http.MethodPut && r.URL.Path == "/api/custom-ai-providers/9":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &put); err != nil {
				t.Errorf("PUT body is not JSON: %v", err)
			}
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "providers")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pd, "p.yaml"), []byte(providerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newProvisionClient(srv.URL, "key", false)
	if _, _, err := applyProviders(c, dir, filepath.Dir(dir), 1); err != nil {
		t.Fatalf("applyProviders: %v", err)
	}
	if put == nil {
		t.Fatal("no PUT was sent")
	}
	return put
}

const providerYAMLBase = `id: 9
name: Quote metadata
slug: quote-metadata
description: d
provider_type: rest
category: data_enrichment
endpoint_url: https://example.test
http_method: POST
enabled: true
`

func TestProviderMapFields_AbsentKeyOmitsField(t *testing.T) {
	put := putPayloadFor(t, providerYAMLBase)
	for _, k := range []string{"response_mappings", "request_headers"} {
		if v, ok := put[k]; ok {
			t.Errorf("%s absent in YAML must be omitted from the payload, got %q", k, v)
		}
	}
}

func TestProviderMapFields_NullValueOmitsField(t *testing.T) {
	put := putPayloadFor(t, providerYAMLBase+"response_mappings:\nrequest_headers: ~\n")
	for _, k := range []string{"response_mappings", "request_headers"} {
		if v, ok := put[k]; ok {
			t.Errorf("%s: null in YAML must be treated as absent, got %q", k, v)
		}
	}
}

func TestProviderMapFields_ExplicitEmptySendsEmptyObject(t *testing.T) {
	put := putPayloadFor(t, providerYAMLBase+"response_mappings: {}\nrequest_headers: {}\n")
	for _, k := range []string{"response_mappings", "request_headers"} {
		if v, ok := put[k]; !ok || v != "{}" {
			t.Errorf("%s: {} in YAML must send \"{}\" to clear the stored value, got %v (present=%v)", k, v, ok)
		}
	}
}

func TestProviderMapFields_NonEmptyUnchanged(t *testing.T) {
	put := putPayloadFor(t, providerYAMLBase+"response_mappings:\n  quote: $.quote\nrequest_headers:\n  X-New: \"2\"\n")
	if got := put["response_mappings"]; got != `{"quote":"$.quote"}` {
		t.Errorf("response_mappings = %v", got)
	}
	if got := put["request_headers"]; got != `{"X-New":"2"}` {
		t.Errorf("request_headers = %v", got)
	}
}

func TestProviderMapFieldChanges(t *testing.T) {
	stored := providerItem{ResponseMappings: `{"quote":"$.quote"}`, RequestHeaders: `{"X-Old":"1"}`}

	t.Run("absent shows no change", func(t *testing.T) {
		if got := providerMapFieldChanges(providerConfig{}, stored); len(got) != 0 {
			t.Errorf("want no changes, got %q", got)
		}
	})
	t.Run("explicit empty against stored mapping is a clearing change", func(t *testing.T) {
		cfg := providerConfig{ResponseMappings: map[string]string{}, RequestHeaders: map[string]string{}}
		got := providerMapFieldChanges(cfg, stored)
		if len(got) != 2 {
			t.Fatalf("want 2 changes, got %q", got)
		}
		for _, line := range got {
			if !strings.Contains(line, "clears") {
				t.Errorf("change line should say it clears: %q", line)
			}
		}
	})
	t.Run("explicit empty against nothing stored is no change", func(t *testing.T) {
		cfg := providerConfig{ResponseMappings: map[string]string{}}
		if got := providerMapFieldChanges(cfg, providerItem{}); len(got) != 0 {
			t.Errorf("want no changes, got %q", got)
		}
		if got := providerMapFieldChanges(cfg, providerItem{ResponseMappings: "{}"}); len(got) != 0 {
			t.Errorf("stored {} vs {}: want no changes, got %q", got)
		}
	})
	t.Run("equal non-empty mapping is no change, key order ignored", func(t *testing.T) {
		cfg := providerConfig{ResponseMappings: map[string]string{"quote": "$.quote"}}
		if got := providerMapFieldChanges(cfg, stored); len(got) != 0 {
			t.Errorf("want no changes, got %q", got)
		}
	})
	t.Run("header values are never printed", func(t *testing.T) {
		cfg := providerConfig{RequestHeaders: map[string]string{"Authorization": "Bearer s3cret-new"}}
		got := providerMapFieldChanges(cfg, providerItem{RequestHeaders: `{"Authorization":"Bearer s3cret-old"}`})
		if len(got) != 1 {
			t.Fatalf("want 1 change, got %q", got)
		}
		if strings.Contains(got[0], "s3cret") {
			t.Errorf("header value leaked into change line: %q", got[0])
		}
	})
	t.Run("different non-empty mapping is a change", func(t *testing.T) {
		cfg := providerConfig{ResponseMappings: map[string]string{"text": "$.text"}}
		got := providerMapFieldChanges(cfg, stored)
		if len(got) != 1 || !strings.Contains(got[0], "response_mappings") {
			t.Errorf("want one response_mappings change, got %q", got)
		}
	})
}

func TestProviderMapFieldChanges_StoredShapeIsNamed(t *testing.T) {
	cfg := providerConfig{ResponseMappings: map[string]string{"quote": "$.quote"}}
	for _, tc := range []struct{ stored, want string }{
		{`{"quote":1}`, "not a string map"},
		{`["quote"]`, "not a JSON object"},
		{`{not json`, "not valid JSON"},
	} {
		got := providerMapFieldChanges(cfg, providerItem{ResponseMappings: tc.stored})
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("stored %s: want one change saying %q, got %q", tc.stored, tc.want, got)
		}
	}
}

func TestProviderMapFieldChanges_InvalidStoredHeadersNeverPrintValue(t *testing.T) {
	cfg := providerConfig{RequestHeaders: map[string]string{"Authorization": "Bearer new"}}
	got := providerMapFieldChanges(cfg, providerItem{RequestHeaders: `{"Authorization":"Bearer s3cret-old"`})
	if len(got) != 1 || !strings.Contains(got[0], "not valid JSON") {
		t.Fatalf("want one change saying the stored value is not valid JSON, got %q", got)
	}
	if strings.Contains(got[0], "s3cret") {
		t.Errorf("stored header value leaked: %q", got[0])
	}
}
