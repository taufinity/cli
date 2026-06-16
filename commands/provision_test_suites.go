// provision_test_suites.go — provision support for test suite resources.
//
// Pattern mirrors provision_playbooks.go: list → name-match → upsert → reconcile cases.
// The suite's target playbook is resolved from a name to a numeric ID at apply
// time, so the YAML never carries environment-specific IDs.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// testSuiteConfig is the YAML shape for a single test suite + its cases.
//
// Playbook example (studio/test-suites/voedingsplan.yaml):
//
//	name: FWC Voedingsplan - Intake Validation
//	target_type: playbook
//	target_playbook: Voedingsplan Generator FWC
//	pass_threshold: 100
//	enabled: true
//	cases:
//	  - name: Eva Shabo - Perimenopauze afvallen 15kg
//	    position: 0
//	    inputs:
//	      intake_json: |
//	        {"naam": "Eva Shabo", ...}
//	    assertions:
//	      - type: verdict_pass
type testSuiteConfig struct {
	Name            string          `yaml:"name"`
	Slug            string          `yaml:"slug,omitempty"` // stable provision key; leading over name when set
	Description     string          `yaml:"description,omitempty"`
	TargetType      string          `yaml:"target_type"`                // "dashboard" | "playbook" | "widget"
	TargetPlaybook  string          `yaml:"target_playbook,omitempty"`  // resolved to numeric ID (playbook name)
	TargetDashboard string          `yaml:"target_dashboard,omitempty"` // resolved to numeric ID (dashboard slug)
	TargetWidget    string          `yaml:"target_widget,omitempty"`    // resolved to numeric ID (chat widget name)
	RunAs           string          `yaml:"run_as,omitempty"`           // dashboard suites: email of a client_group user to run AS
	PassThreshold   float64         `yaml:"pass_threshold,omitempty"`
	Enabled         *bool           `yaml:"enabled,omitempty"`
	Priority        string          `yaml:"priority,omitempty"` // must | recommend | optional (local-only, never sent to API)
	Cases           []testCaseEntry `yaml:"cases,omitempty"`
}

// testCaseEntry is a single test case within a test suite YAML.
type testCaseEntry struct {
	Name       string              `yaml:"name"`
	Position   int                 `yaml:"position"`
	Enabled    *bool               `yaml:"enabled,omitempty"`
	Inputs     map[string]any      `yaml:"inputs,omitempty"`   // for playbook cases
	Filters    map[string]string   `yaml:"filters,omitempty"`  // for dashboard cases; produces {"filters":{...}}
	Question   string              `yaml:"question,omitempty"` // for widget cases; produces {"question":"..."}
	Assertions []testCaseAssertion `yaml:"assertions,omitempty"`
}

// testCaseAssertion mirrors the JSON Assertion struct for YAML decoding.
type testCaseAssertion struct {
	Type        string `yaml:"type"         json:"type"`
	Value       int    `yaml:"value"        json:"value,omitempty"`
	Name        string `yaml:"name"         json:"name,omitempty"`
	Field       string `yaml:"field"        json:"field,omitempty"`
	StringValue string `yaml:"string_value" json:"string_value,omitempty"`
	Max         int    `yaml:"max"          json:"max,omitempty"`
	StepKey     string `yaml:"step_key"     json:"step_key,omitempty"`
}

// testSuiteListItem is the minimal shape we need from GET /api/test-suites/.
type testSuiteListItem struct {
	ID         uint   `json:"id"`
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	TargetType string `json:"target_type"`
	Slug       string `json:"slug,omitempty"`
}

// testCaseRemote is the shape of a case returned by GET /api/test-suites/{uuid}.
type testCaseRemote struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// applyTestSuites applies all test suite YAML files from dir to the org.
func applyTestSuites(c *provisionClient, dir string, orgID uint) error {
	td := filepath.Join(dir, "test-suites")
	if !fileExists(td) {
		return nil
	}
	entries, err := os.ReadDir(td)
	if err != nil {
		return fmt.Errorf("read test-suites dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		tf := filepath.Join(td, e.Name())
		var cfg testSuiteConfig
		mustReadYAML(tf, &cfg)
		if err := upsertTestSuite(c, orgID, cfg, tf); err != nil {
			return fmt.Errorf("test-suite %s: %w", e.Name(), err)
		}
	}
	return nil
}

// upsertTestSuite creates or updates a test suite + its cases for the given org.
// Match key: slug (when present in YAML) or case-insensitive name (fallback).
// yamlPath is used to write the slug back after first CREATE.
func upsertTestSuite(c *provisionClient, orgID uint, cfg testSuiteConfig, yamlPath string) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("test_suite: name is required")
	}
	if cfg.TargetType == "" {
		cfg.TargetType = "dashboard"
	}

	body, status, err := c.getForOrg("/test-suites/", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list test suites", status, body, err)
	}
	var existing []testSuiteListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return fmt.Errorf("parse test suites: %w (body=%s)", err, provisionSummarize(body))
	}
	var matches []testSuiteListItem
	for _, s := range existing {
		if cfg.Slug != "" {
			if s.Slug == cfg.Slug {
				matches = append(matches, s)
			}
		} else {
			if strings.EqualFold(s.Name, cfg.Name) {
				matches = append(matches, s)
			}
		}
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = fmt.Sprintf("uuid=%s", m.UUID)
		}
		return fmt.Errorf("test_suite %q: %d matches (%s) — ambiguous, refusing to apply",
			cfg.Name, len(matches), strings.Join(ids, ", "))
	}

	// Resolve target → numeric ID.
	var targetID *uint
	switch {
	case cfg.TargetType == "playbook" && cfg.TargetPlaybook != "":
		pbID, err := resolveTestSuitePlaybookID(c, orgID, cfg.TargetPlaybook)
		if err != nil {
			return fmt.Errorf("test_suite %q: %w", cfg.Name, err)
		}
		targetID = &pbID
	case (cfg.TargetType == "dashboard" || cfg.TargetType == "") && cfg.TargetDashboard != "":
		ddID, err := resolveTestSuiteDashboardDefID(c, orgID, cfg.TargetDashboard)
		if err != nil {
			return fmt.Errorf("test_suite %q: %w", cfg.Name, err)
		}
		targetID = &ddID
	case cfg.TargetType == "widget" && cfg.TargetWidget != "":
		wID, err := resolveTestSuiteWidgetID(c, orgID, cfg.TargetWidget)
		if err != nil {
			return fmt.Errorf("test_suite %q: %w", cfg.Name, err)
		}
		targetID = &wID
	}

	// Resolve run_as email → user_id.
	var runAsUserID *uint
	if cfg.RunAs != "" {
		uid, err := lookupUserIDByEmail(c, cfg.RunAs)
		if err != nil {
			return fmt.Errorf("test_suite %q: resolve run_as %q: %w", cfg.Name, cfg.RunAs, err)
		}
		runAsUserID = &uid
	}

	passThreshold := 80.0
	if cfg.PassThreshold > 0 {
		passThreshold = cfg.PassThreshold
	}
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}

	var suiteUUID string

	if len(matches) == 1 {
		suiteUUID = matches[0].UUID
		fmt.Printf("UPDATE test_suite %q uuid=%s\n", cfg.Name, suiteUUID)
		updatePayload := map[string]any{
			"name":           cfg.Name,
			"description":    cfg.Description,
			"pass_threshold": passThreshold,
			"enabled":        enabled,
			"run_as_user_id": runAsUserID,
		}
		payload, _ := json.Marshal(updatePayload)
		rb, st, err := c.writeForOrg("PUT", "/test-suites/"+suiteUUID, payload, orgID)
		if err != nil || st >= 300 {
			return provisionAPIErr(fmt.Sprintf("update test_suite %q", cfg.Name), st, rb, err)
		}
	} else {
		fmt.Printf("CREATE test_suite %q\n", cfg.Name)
		createPayload := map[string]any{
			"name":           cfg.Name,
			"description":    cfg.Description,
			"target_type":    cfg.TargetType,
			"pass_threshold": passThreshold,
			"enabled":        enabled,
		}
		if targetID != nil {
			createPayload["target_id"] = *targetID
		}
		if runAsUserID != nil {
			createPayload["run_as_user_id"] = *runAsUserID
		}
		payload, _ := json.Marshal(createPayload)
		rb, st, err := c.writeForOrg("POST", "/test-suites/", payload, orgID)
		if err != nil || st >= 300 {
			return provisionAPIErr(fmt.Sprintf("create test_suite %q", cfg.Name), st, rb, err)
		}
		var created struct {
			UUID string `json:"uuid"`
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(rb, &created); err != nil || created.UUID == "" {
			if c.dryRun {
				fmt.Printf("[dry-run] skipping case sync for test_suite %q (no UUID returned)\n", cfg.Name)
				return nil
			}
			return fmt.Errorf("create test_suite %q: response missing uuid (body=%s)", cfg.Name, provisionSummarize(rb))
		}
		suiteUUID = created.UUID
		if created.Slug != "" {
			if err := pinSlug(yamlPath, cfg.Slug, created.Slug); err != nil {
				fmt.Printf("  WARN: could not pin slug for test_suite %q: %v\n", cfg.Name, err)
			}
		}
	}

	return reconcileTestCases(c, orgID, suiteUUID, cfg.Cases)
}

// resolveTestSuitePlaybookID looks up a playbook by name in the org and returns its ID.
func resolveTestSuitePlaybookID(c *provisionClient, orgID uint, name string) (uint, error) {
	body, status, err := c.getForOrg("/playbooks/", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list playbooks (for test_suite target resolution)", status, body, err)
	}
	var playbooks []provisionPlaybookListItem
	if err := unmarshalListEnvelope(body, &playbooks); err != nil {
		return 0, fmt.Errorf("parse playbooks: %w", err)
	}
	for _, p := range playbooks {
		if strings.EqualFold(p.Name, name) {
			return p.ID, nil
		}
	}
	return 0, fmt.Errorf("target_playbook %q not found in org — provision playbooks first", name)
}

// resolveTestSuiteDashboardDefID looks up a dashboard definition by slug.
func resolveTestSuiteDashboardDefID(c *provisionClient, orgID uint, slug string) (uint, error) {
	body, status, err := c.getForOrg("/admin/dashboard-definitions", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list dashboard definitions (for test_suite target resolution)", status, body, err)
	}
	var envelope struct {
		Definitions []struct {
			ID   uint   `json:"id"`
			Slug string `json:"slug"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, fmt.Errorf("parse dashboard definitions: %w", err)
	}
	for _, d := range envelope.Definitions {
		if strings.EqualFold(d.Slug, slug) {
			return d.ID, nil
		}
	}
	return 0, fmt.Errorf("target_dashboard %q not found in org — provision dashboards first", slug)
}

// resolveTestSuiteWidgetID looks up a chat widget by name in the org and returns its ID.
func resolveTestSuiteWidgetID(c *provisionClient, orgID uint, name string) (uint, error) {
	body, status, err := c.getForOrg("/widgets/", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list widgets (for test_suite target resolution)", status, body, err)
	}
	var widgets []widgetListItem
	if err := unmarshalListEnvelope(body, &widgets); err != nil {
		return 0, fmt.Errorf("parse widgets: %w", err)
	}
	for _, w := range widgets {
		if strings.EqualFold(w.Name, name) {
			return w.ID, nil
		}
	}
	return 0, fmt.Errorf("target_widget %q not found in org — provision widgets first", name)
}

// reconcileTestCases syncs the cases list to match cfg.Cases.
func reconcileTestCases(c *provisionClient, orgID uint, suiteUUID string, want []testCaseEntry) error {
	body, status, err := c.getForOrg("/test-suites/"+suiteUUID, orgID)
	if err != nil || status != 200 {
		return provisionAPIErr(fmt.Sprintf("get test_suite %s", suiteUUID), status, body, err)
	}
	var withCases struct {
		Cases []testCaseRemote `json:"cases"`
	}
	if err := json.Unmarshal(body, &withCases); err != nil {
		return fmt.Errorf("parse test_suite cases: %w (body=%s)", err, provisionSummarize(body))
	}

	byName := make(map[string]testCaseRemote, len(withCases.Cases))
	for _, tc := range withCases.Cases {
		byName[strings.ToLower(tc.Name)] = tc
	}

	wantNames := make(map[string]bool, len(want))
	for _, tc := range want {
		key := strings.ToLower(tc.Name)
		wantNames[key] = true

		var (
			inputJSON string
			inputErr  error
		)
		switch {
		case tc.Question != "":
			inputJSON, inputErr = buildWidgetCaseInput(tc)
		case len(tc.Filters) > 0:
			inputJSON, inputErr = buildDashboardCaseInput(tc)
		default:
			inputJSON, inputErr = buildPlaybookCaseInput(tc)
		}
		if inputErr != nil {
			return fmt.Errorf("case %q: %w", tc.Name, inputErr)
		}

		assertJSON, err := buildAssertionsJSON(tc.Assertions)
		if err != nil {
			return fmt.Errorf("case %q assertions: %w", tc.Name, err)
		}

		enabled := true
		if tc.Enabled != nil {
			enabled = *tc.Enabled
		}

		payload, _ := json.Marshal(map[string]any{
			"name":       tc.Name,
			"position":   tc.Position,
			"input":      inputJSON,
			"assertions": assertJSON,
			"enabled":    enabled,
		})

		if cur, found := byName[key]; found {
			fmt.Printf("  UPDATE case %q id=%d\n", tc.Name, cur.ID)
			rb, st, err := c.writeForOrg("PUT",
				fmt.Sprintf("/test-suites/%s/cases/%d", suiteUUID, cur.ID), payload, orgID)
			if err != nil || st >= 300 {
				return provisionAPIErr(fmt.Sprintf("update case %q", tc.Name), st, rb, err)
			}
		} else {
			fmt.Printf("  CREATE case %q position=%d\n", tc.Name, tc.Position)
			rb, st, err := c.writeForOrg("POST",
				fmt.Sprintf("/test-suites/%s/cases", suiteUUID), payload, orgID)
			if err != nil || st >= 300 {
				return provisionAPIErr(fmt.Sprintf("create case %q", tc.Name), st, rb, err)
			}
		}
	}

	// Delete cases that are no longer in the YAML.
	for _, cur := range withCases.Cases {
		if !wantNames[strings.ToLower(cur.Name)] {
			fmt.Printf("  DELETE case %q id=%d (not in YAML)\n", cur.Name, cur.ID)
			rb, st, err := c.writeForOrg("DELETE",
				fmt.Sprintf("/test-suites/%s/cases/%d", suiteUUID, cur.ID), nil, orgID)
			if err != nil || (st >= 300 && st != 404) {
				return provisionAPIErr(fmt.Sprintf("delete case %q", cur.Name), st, rb, err)
			}
		}
	}
	return nil
}

// buildPlaybookCaseInput marshals the case's inputs map into a PlaybookInput JSON string.
// The output is a JSON object whose keys map directly to the playbook runner's formData —
// e.g. {"intake": {"klant_naam": "...", ...}} so RunPlaybookForTest can type-assert the
// value to map[string]any and MergeFormDataIntoInputs serialises it to a JSON string for
// CustomMetadata, exactly matching the real trigger's form_data shape.
//
// Contract: YAML intake values may be multi-line JSON strings or nested YAML maps.
// Either way they are parsed and stored as nested JSON objects, not as quoted strings —
// because executePlaybookCase type-asserts formData["intake"].(map[string]any).
// Use the step's intake_key name (typically "intake") not "intake_json".
func buildPlaybookCaseInput(tc testCaseEntry) (string, error) {
	if len(tc.Inputs) == 0 {
		return "{}", nil
	}
	// Build a map[string]any so values can be proper nested JSON objects.
	output := make(map[string]any, len(tc.Inputs))
	for k, v := range tc.Inputs {
		var raw string
		switch sv := convertYAMLValue(v).(type) {
		case string:
			raw = sv
		default:
			b, err := json.Marshal(sv)
			if err != nil {
				return "", fmt.Errorf("input key %q: cannot marshal value: %w", k, err)
			}
			raw = string(b)
		}
		// Attempt to parse the raw value as JSON so it becomes a nested object
		// (not a quoted string). The playbook runner's executePlaybookCase does a
		// type assertion formData["intake"].(map[string]any) — if the value is a
		// string the assertion fails, materializeAttachments corrupts the formData,
		// and all steps skip. Storing as a parsed object prevents this.
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			output[k] = parsed
		} else {
			output[k] = raw // leave as string when it's not valid JSON
		}
	}
	b, err := json.Marshal(output)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildDashboardCaseInput produces the DashboardInput JSON {"filters":{...}}.
func buildDashboardCaseInput(tc testCaseEntry) (string, error) {
	b, err := json.Marshal(map[string]any{"filters": tc.Filters})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildWidgetCaseInput produces the WidgetInput JSON {"question":"..."}.
func buildWidgetCaseInput(tc testCaseEntry) (string, error) {
	b, err := json.Marshal(map[string]any{"question": tc.Question})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildAssertionsJSON marshals the YAML assertion list to a JSON string.
func buildAssertionsJSON(assertions []testCaseAssertion) (string, error) {
	if len(assertions) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(assertions)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
