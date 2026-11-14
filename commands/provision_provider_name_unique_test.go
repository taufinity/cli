package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// duplicateNameServer stands up a fake Studio holding two enabled providers
// that share a name and differ only in the dataset they point at. This is the
// WVS org 18 shape: four rows called "WVS BQ Tracking", three on staging's
// dataset and one on production's.
func duplicateNameServer(t *testing.T, puts *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/custom-ai-providers":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 23, "name": "WVS BQ Tracking", "endpoint_url": "https://staging.test"},
				{"id": 25, "name": "WVS BQ Tracking", "endpoint_url": "https://prod.test"},
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/custom-ai-providers/"):
			*puts = append(*puts, r.URL.Path)
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/custom-ai-providers":
			*puts = append(*puts, "POST")
			w.Write([]byte(`{"id":99}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func applyProviderSpec(t *testing.T, srv *httptest.Server, providerYAML string) error {
	t.Helper()
	dir := t.TempDir()
	pd := filepath.Join(dir, "providers")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pd, "p.yaml"), []byte(providerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newProvisionClient(srv.URL, "key", false)
	_, _, err := applyProviders(c, dir, filepath.Dir(dir), 1)
	return err
}

const dupNameSpecNoKey = `name: WVS BQ Tracking
description: d
provider_type: rest
category: data_enrichment
endpoint_url: https://example.test
http_method: POST
enabled: true
`

// A spec that names a provider by name alone, where two rows carry that name,
// must be refused. Before this, provision matched whichever row the API listed
// first, updated that one, and left the others: which dataset a quote's
// analytics reached was decided by row order, and the spec gave no sign of it.
func TestUpsertProvider_AmbiguousNameIsRefused(t *testing.T) {
	var puts []string
	srv := duplicateNameServer(t, &puts)
	defer srv.Close()

	err := applyProviderSpec(t, srv, dupNameSpecNoKey)
	if err == nil {
		t.Fatal("a name matching two providers must be refused, got no error")
	}
	// The operator has to be able to act on the message, which means knowing
	// which rows collided and what to do about it.
	for _, want := range []string{"id=23", "id=25", "WVS BQ Tracking"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the operator can find the rows; got: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "slug") || !strings.Contains(err.Error(), "id") {
		t.Errorf("error must say how to resolve it (add id or slug); got: %v", err)
	}
	// Refusing means writing nothing. A partial apply is worse than a refusal:
	// it would update one of the duplicates and report an error at the same time.
	if len(puts) != 0 {
		t.Errorf("refusal must write nothing, but provision sent %v", puts)
	}
}

// The refusal is narrow: it only fires when name is the only way to match. A
// spec that pins the row by id still applies, duplicates or not, because an id
// names exactly one row.
func TestUpsertProvider_DuplicateNamesStillApplyWhenIDPinned(t *testing.T) {
	var puts []string
	srv := duplicateNameServer(t, &puts)
	defer srv.Close()

	if err := applyProviderSpec(t, srv, "id: 25\n"+dupNameSpecNoKey); err != nil {
		t.Fatalf("an id-pinned spec must apply even with duplicate names: %v", err)
	}
	if len(puts) != 1 || puts[0] != "/api/custom-ai-providers/25" {
		t.Errorf("must update exactly the pinned row 25, got %v", puts)
	}
}

// A name that matches exactly one row is the ordinary case and must keep
// working: this is the guard against the refusal being too broad.
func TestUpsertProvider_UniqueNameStillApplies(t *testing.T) {
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/custom-ai-providers":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 23, "name": "WVS BQ Tracking"},
				{"id": 31, "name": "Iets anders"},
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/custom-ai-providers/"):
			puts = append(puts, r.URL.Path)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := applyProviderSpec(t, srv, dupNameSpecNoKey); err != nil {
		t.Fatalf("a unique name must still apply: %v", err)
	}
	if len(puts) != 1 || puts[0] != "/api/custom-ai-providers/23" {
		t.Errorf("must update row 23, got %v", puts)
	}
}
