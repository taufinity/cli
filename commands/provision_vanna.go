// provision_vanna.go — org-level training data for the natural-language-to-SQL
// assistant (Vanna).
//
// One file per org: <dir>/vanna-training.yaml. Three entry kinds, all optional:
//
//	ddl:
//	  - id: mart-orders-daily          # stable key for idempotent upserts
//	    table: analytics.mart_orders_daily
//	    ddl: |
//	      CREATE TABLE `analytics.mart_orders_daily` (...)
//
//	examples:
//	  - id: revenue-last-week
//	    question: "What was revenue last week?"
//	    sql: "SELECT SUM(revenue) FROM analytics.mart_orders_daily WHERE ..."
//
//	glossary:
//	  - id: aov-definition
//	    term: "AOV"
//	    definition: "Average order value — revenue divided by order count."
//
// `id` is optional but strongly recommended: it becomes the entry's unique key,
// which is what makes a re-run an upsert instead of an insert. Entries without
// an id are created as new rows every time provision runs.
//
// After the entries are pushed, one retrain call rebuilds the org's vector
// collection from the current database state, so the assistant actually sees
// the new training data.
package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
)

// vannaTrainingConfig is the YAML shape of vanna-training.yaml.
type vannaTrainingConfig struct {
	DDL      []vannaDDLEntry      `yaml:"ddl"`
	Examples []vannaExampleEntry  `yaml:"examples"`
	Glossary []vannaGlossaryEntry `yaml:"glossary"`
}

type vannaDDLEntry struct {
	ID    string `yaml:"id,omitempty"`
	Table string `yaml:"table"`
	DDL   string `yaml:"ddl"`
}

type vannaExampleEntry struct {
	ID       string `yaml:"id,omitempty"`
	Question string `yaml:"question"`
	SQL      string `yaml:"sql"`
}

type vannaGlossaryEntry struct {
	ID         string `yaml:"id,omitempty"`
	Term       string `yaml:"term"`
	Definition string `yaml:"definition"`
}

// vannaTrainingRequest is the JSON body for POST /api/vanna-training/.
type vannaTrainingRequest struct {
	EntryType  string  `json:"entry_type"`
	UniqueKey  *string `json:"unique_key,omitempty"`
	DDL        *string `json:"ddl,omitempty"`
	Question   *string `json:"question,omitempty"`
	SQL        *string `json:"sql,omitempty"`
	Term       *string `json:"term,omitempty"`
	Definition *string `json:"definition,omitempty"`
}

// applyVannaTraining applies <dir>/vanna-training.yaml. Missing file is a no-op.
func applyVannaTraining(c *provisionClient, dir string, orgID uint) error {
	path := filepath.Join(dir, "vanna-training.yaml")
	if !fileExists(path) {
		return nil
	}
	var cfg vannaTrainingConfig
	mustReadYAML(path, &cfg)
	return upsertVannaTraining(c, orgID, cfg)
}

// upsertVannaTraining pushes every entry, then triggers one retrain.
//
// A single bad entry should not abort the whole run — one malformed example
// would otherwise block the rest of the training set from landing — so failures
// are warned about, counted, and reported as one error at the end.
func upsertVannaTraining(c *provisionClient, orgID uint, cfg vannaTrainingConfig) error {
	pushed, failed := 0, 0

	push := func(label string, req vannaTrainingRequest) {
		if err := postVannaEntry(c, orgID, req); err != nil {
			c.Warn("vanna-training %s: %v", label, err)
			failed++
			return
		}
		pushed++
	}

	for _, e := range cfg.DDL {
		ddl := e.DDL
		req := vannaTrainingRequest{EntryType: "ddl", DDL: &ddl}
		if e.ID != "" {
			req.UniqueKey = &e.ID
		}
		push(fmt.Sprintf("ddl %q", e.Table), req)
	}

	for _, e := range cfg.Examples {
		q, s := e.Question, e.SQL
		req := vannaTrainingRequest{EntryType: "qa", Question: &q, SQL: &s}
		if e.ID != "" {
			req.UniqueKey = &e.ID
		}
		push(fmt.Sprintf("example %q", truncateLabel(e.Question, 40)), req)
	}

	for _, e := range cfg.Glossary {
		t, d := e.Term, e.Definition
		req := vannaTrainingRequest{EntryType: "glossary", Term: &t, Definition: &d}
		if e.ID != "" {
			req.UniqueKey = &e.ID
		}
		push(fmt.Sprintf("glossary %q", e.Term), req)
	}

	fmt.Printf("provision: vanna-training: %d entries pushed (%d failed)\n", pushed, failed)

	// Retraining an unchanged collection is wasted work, so only retrain when
	// at least one entry landed.
	if pushed > 0 {
		if err := triggerVannaRetrain(c, orgID); err != nil {
			c.Warn("vanna-training retrain: %v", err)
		}
	}

	if failed > 0 {
		return fmt.Errorf("vanna-training: %d entries failed to push", failed)
	}
	return nil
}

func postVannaEntry(c *provisionClient, orgID uint, req vannaTrainingRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	respBody, status, err := c.writeForOrg(http.MethodPost, "/vanna-training/", body, orgID)
	if err != nil || status >= 300 {
		return provisionAPIErr("push entry", status, respBody, err)
	}
	return nil
}

func triggerVannaRetrain(c *provisionClient, orgID uint) error {
	respBody, status, err := c.writeForOrg(http.MethodPost, "/vanna-training/retrain", []byte(`{}`), orgID)
	if err != nil || status >= 300 {
		return provisionAPIErr("retrain", status, respBody, err)
	}
	var result struct {
		EntriesRetrained int `json:"entries_retrained"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil && result.EntriesRetrained > 0 {
		fmt.Printf("provision: vanna-training: retrain complete — %d entries indexed\n", result.EntriesRetrained)
	}
	return nil
}

func truncateLabel(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
