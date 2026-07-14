package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type vannaCall struct {
	Path  string
	OrgID string
	Body  map[string]any
}

func vannaServer(t *testing.T, calls *[]vannaCall, mu *sync.Mutex, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(b, &parsed)
		mu.Lock()
		*calls = append(*calls, vannaCall{Path: r.URL.Path, OrgID: r.Header.Get("X-Organization-ID"), Body: parsed})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/vanna-training/retrain" {
			_, _ = w.Write([]byte(`{"entries_retrained":3}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
}

const vannaYAML = `
ddl:
  - id: mart-orders-daily
    table: analytics.mart_orders_daily
    ddl: |
      CREATE TABLE analytics.mart_orders_daily (day DATE, orders INT64);
examples:
  - id: revenue-last-week
    question: "What was revenue last week?"
    sql: "SELECT SUM(revenue) FROM analytics.mart_orders_daily"
glossary:
  - id: aov-definition
    term: AOV
    definition: "Average order value."
`

func writeVannaYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vanna-training.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyVannaTrainingHappyPath(t *testing.T) {
	var mu sync.Mutex
	var calls []vannaCall
	ts := vannaServer(t, &calls, &mu, http.StatusCreated)
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	if err := applyVannaTraining(c, writeVannaYAML(t, vannaYAML), 5); err != nil {
		t.Fatalf("applyVannaTraining: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// 3 entries + 1 retrain.
	if len(calls) != 4 {
		t.Fatalf("got %d call(s), want 4: %+v", len(calls), calls)
	}
	if calls[3].Path != "/api/vanna-training/retrain" {
		t.Errorf("last call should be the retrain, got %s", calls[3].Path)
	}
	types := map[string]bool{}
	for _, c := range calls[:3] {
		if c.Path != "/api/vanna-training/" {
			t.Errorf("entry posted to %s", c.Path)
		}
		if c.OrgID != "5" {
			t.Errorf("X-Organization-ID = %q, want 5", c.OrgID)
		}
		et, _ := c.Body["entry_type"].(string)
		types[et] = true
		if c.Body["unique_key"] == nil {
			t.Errorf("entry_type=%s lost its unique_key — re-running would insert duplicates", et)
		}
	}
	for _, want := range []string{"ddl", "qa", "glossary"} {
		if !types[want] {
			t.Errorf("no entry of type %q was pushed", want)
		}
	}
}

func TestApplyVannaTrainingDryRunNoWrites(t *testing.T) {
	var mu sync.Mutex
	var calls []vannaCall
	ts := vannaServer(t, &calls, &mu, http.StatusCreated)
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", true) // dry-run
	if err := applyVannaTraining(c, writeVannaYAML(t, vannaYAML), 5); err != nil {
		t.Fatalf("dry-run applyVannaTraining: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("dry-run issued %d call(s), want 0: %+v", len(calls), calls)
	}
}

func TestApplyVannaTrainingMissingFileIsNoOp(t *testing.T) {
	c := newProvisionClient("http://127.0.0.1:1", "key", false)
	if err := applyVannaTraining(c, t.TempDir(), 5); err != nil {
		t.Fatalf("missing vanna-training.yaml should be a no-op, got %v", err)
	}
}

// A rejected entry must not abort the run, but it must surface as an error at
// the end — otherwise a partially-loaded training set looks like a success.
func TestApplyVannaTrainingReportsRejectedEntries(t *testing.T) {
	var mu sync.Mutex
	var calls []vannaCall
	ts := vannaServer(t, &calls, &mu, http.StatusBadRequest)
	defer ts.Close()

	c := newProvisionClient(ts.URL, "key", false)
	err := applyVannaTraining(c, writeVannaYAML(t, vannaYAML), 5)
	if err == nil {
		t.Fatal("expected an error when every entry is rejected")
	}
	mu.Lock()
	defer mu.Unlock()
	// All three entries attempted; no retrain, because nothing landed.
	if len(calls) != 3 {
		t.Errorf("got %d call(s), want 3 (no retrain after a fully failed push): %+v", len(calls), calls)
	}
}
