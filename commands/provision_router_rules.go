// provision_router_rules.go — provision support for router rules.
//
// Phase B2 activates this handler. It now:
//  1. Auto-creates a knowledge_file source-type router for the org if
//     one doesn't exist (one router per org per source_type is enough
//     — each carries N rules).
//  2. Resolves the dispatch playbook by name → ID.
//  3. Translates YAML conditions to the JSON config blob expected by
//     the internal/router knowledge_file_match evaluator.
//  4. Match-by-Name on rules within the router; PUT existing or POST
//     new. Uniqueness is enforced server-side by migration 184's
//     partial unique index on (router_id, name).
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// routerRuleConfig is the YAML shape for a router rule.
// See studio/routers/<rule>.yaml in the customer template.
type routerRuleConfig struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description,omitempty"`
	SourceType  string                `yaml:"source_type"`
	RuleType    string                `yaml:"rule_type,omitempty"` // widget source only: keyword, regex, match_any
	Keywords    []string              `yaml:"keywords,omitempty"`  // widget keyword rule
	Event       string                `yaml:"event,omitempty"`
	Conditions  []routerRuleCondition `yaml:"conditions,omitempty"`
	Dispatch    routerRuleDispatch    `yaml:"dispatch"`
	OnError     *routerRuleOnError    `yaml:"on_error,omitempty"`
	Enabled     *bool                 `yaml:"enabled,omitempty"`
	StopOnMatch *bool                 `yaml:"stop_on_match,omitempty"`
}

type routerRuleCondition struct {
	Field string `yaml:"field"`
	Op    string `yaml:"op"`
	Value any    `yaml:"value"`
}

type routerRuleDispatch struct {
	Playbook string `yaml:"playbook"`
}

type routerRuleOnError struct {
	Retry        int    `yaml:"retry,omitempty"`
	Backoff      string `yaml:"backoff,omitempty"`
	OnExhaustion string `yaml:"on_exhaustion,omitempty"`
}

// routerListItem mirrors the subset of GET /api/routers/ we need.
type routerListItem struct {
	ID         uint               `json:"id"`
	Name       string             `json:"name"`
	SourceType string             `json:"source_type"`
	Enabled    bool               `json:"enabled"`
	Rules      []routerRuleRemote `json:"rules,omitempty"`
}

// routerRuleRemote mirrors a rule row returned from GET /api/routers/{id}.
type routerRuleRemote struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	Position   int    `json:"position"`
	RuleType   string `json:"rule_type"`
	PlaybookID uint   `json:"playbook_id"`
	Enabled    bool   `json:"enabled"`
}

// applyRouterRules applies all router rule YAML files from the routers/ subdirectory.
func applyRouterRules(c *provisionClient, dir string, orgID uint) error {
	rd := filepath.Join(dir, "routers")
	if !fileExists(rd) {
		return nil
	}
	entries, err := os.ReadDir(rd)
	if err != nil {
		return fmt.Errorf("read routers dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		rf := filepath.Join(rd, e.Name())
		var cfg routerRuleConfig
		mustReadYAML(rf, &cfg)
		if err := upsertRouterRule(c, orgID, cfg); err != nil {
			return fmt.Errorf("router-rule %s: %w", e.Name(), err)
		}
	}
	return nil
}

// upsertRouterRule installs (or updates) one rule under the org's router,
// creating the router if needed. Dispatches to the correct handler by source_type.
func upsertRouterRule(c *provisionClient, orgID uint, cfg routerRuleConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("router rule: name is required")
	}
	if strings.TrimSpace(cfg.SourceType) == "" {
		return fmt.Errorf("router rule %q: source_type is required", cfg.Name)
	}
	if strings.TrimSpace(cfg.Dispatch.Playbook) == "" {
		return fmt.Errorf("router rule %q: dispatch.playbook is required", cfg.Name)
	}
	for i, cond := range cfg.Conditions {
		if strings.TrimSpace(cond.Field) == "" || strings.TrimSpace(cond.Op) == "" {
			return fmt.Errorf("router rule %q: condition[%d] missing field or op", cfg.Name, i)
		}
	}
	switch cfg.SourceType {
	case "knowledge_file":
		return upsertKnowledgeFileRule(c, orgID, cfg)
	case "widget":
		return upsertWidgetKeywordRule(c, orgID, cfg)
	default:
		return fmt.Errorf("router rule %q: source_type %q not supported by cmd/provision (knowledge_file, widget)", cfg.Name, cfg.SourceType)
	}
}

// upsertKnowledgeFileRule handles knowledge_file source type rules.
func upsertKnowledgeFileRule(c *provisionClient, orgID uint, cfg routerRuleConfig) error {
	// 1. Ensure the org has a knowledge_file router.
	routerID, err := ensureKnowledgeFileRouter(c, orgID)
	if err != nil {
		return fmt.Errorf("router rule %q: ensure router: %w", cfg.Name, err)
	}

	// 2. Resolve dispatch playbook name → ID.
	playbookID, err := resolvePlaybookIDByName(c, orgID, cfg.Dispatch.Playbook)
	if err != nil {
		return fmt.Errorf("router rule %q: %w", cfg.Name, err)
	}

	// 3. Translate YAML conditions to the JSON config blob.
	configJSON, err := buildKnowledgeFileMatchConfig(cfg.Conditions)
	if err != nil {
		return fmt.Errorf("router rule %q: build config: %w", cfg.Name, err)
	}

	return upsertRule(c, orgID, routerID, cfg, "knowledge_file_match", string(configJSON), playbookID)
}

// upsertWidgetKeywordRule handles widget source type rules with keyword/regex/match_any evaluators.
func upsertWidgetKeywordRule(c *provisionClient, orgID uint, cfg routerRuleConfig) error {
	ruleType := cfg.RuleType
	if ruleType == "" {
		ruleType = "keyword"
	}
	if ruleType != "keyword" && ruleType != "regex" && ruleType != "match_any" {
		return fmt.Errorf("router rule %q: rule_type %q not supported for widget source (keyword, regex, match_any)", cfg.Name, ruleType)
	}
	if ruleType == "keyword" && len(cfg.Keywords) == 0 {
		return fmt.Errorf("router rule %q: keywords required for keyword rule_type", cfg.Name)
	}

	routerID, err := ensureWidgetRouter(c, orgID)
	if err != nil {
		return fmt.Errorf("router rule %q: ensure widget router: %w", cfg.Name, err)
	}

	playbookID, err := resolvePlaybookIDByName(c, orgID, cfg.Dispatch.Playbook)
	if err != nil {
		return fmt.Errorf("router rule %q: %w", cfg.Name, err)
	}

	configJSON, _ := json.Marshal(map[string]any{"keywords": cfg.Keywords, "case_sensitive": false})
	return upsertRule(c, orgID, routerID, cfg, ruleType, string(configJSON), playbookID)
}

// upsertRule is the shared PUT/POST path for both rule types.
func upsertRule(c *provisionClient, orgID, routerID uint, cfg routerRuleConfig, ruleType, configJSON string, playbookID uint) error {
	rules, err := listRouterRules(c, routerID, orgID)
	if err != nil {
		return fmt.Errorf("router rule %q: list rules: %w", cfg.Name, err)
	}

	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	stopOnMatch := false
	if cfg.StopOnMatch != nil {
		stopOnMatch = *cfg.StopOnMatch
	}

	body := map[string]any{
		"name":          cfg.Name,
		"rule_type":     ruleType,
		"config":        configJSON,
		"playbook_id":   playbookID,
		"stop_on_match": stopOnMatch,
		"enabled":       enabled,
		"position":      0,
	}

	for _, existing := range rules {
		if strings.EqualFold(existing.Name, cfg.Name) {
			fmt.Printf("UPDATE router-rule %q org=%d router=%d rule=%d (dispatch_id=%d)\n",
				cfg.Name, orgID, routerID, existing.ID, playbookID)
			payload, _ := json.Marshal(body)
			_, status, err := c.writeForOrg("PUT", fmt.Sprintf("/routers/%d/rules/%d", routerID, existing.ID), payload, orgID)
			if err != nil || status >= 300 {
				return fmt.Errorf("router rule %q: update status=%d err=%v", cfg.Name, status, err)
			}
			return nil
		}
	}

	fmt.Printf("CREATE router-rule %q org=%d router=%d (rule_type=%s dispatch_id=%d enabled=%v)\n",
		cfg.Name, orgID, routerID, ruleType, playbookID, enabled)
	payload, _ := json.Marshal(body)
	_, status, err := c.writeForOrg("POST", fmt.Sprintf("/routers/%d/rules", routerID), payload, orgID)
	if err != nil || status >= 300 {
		return fmt.Errorf("router rule %q: create status=%d err=%v", cfg.Name, status, err)
	}
	return nil
}

// ensureWidgetRouter returns the ID of the org's widget router, creating one if absent.
func ensureWidgetRouter(c *provisionClient, orgID uint) (uint, error) {
	body, status, err := c.getForOrg("/routers/", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list routers", status, body, err)
	}
	var existing []routerListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return 0, fmt.Errorf("parse routers: %w (body=%s)", err, provisionSummarize(body))
	}
	for _, r := range existing {
		if r.SourceType == "widget" {
			return r.ID, nil
		}
	}
	create := map[string]any{
		"name":          "Widget events (auto)",
		"source_type":   "widget",
		"source_config": "{}",
	}
	fmt.Printf("CREATE router source=widget org=%d (auto)\n", orgID)
	payload, _ := json.Marshal(create)
	respBody, status, err := c.writeForOrg("POST", "/routers/", payload, orgID)
	if err != nil || status >= 300 {
		return 0, fmt.Errorf("create widget router status=%d err=%v body=%s", status, err, provisionSummarize(respBody))
	}
	var created routerListItem
	if err := json.Unmarshal(respBody, &created); err != nil {
		return 0, fmt.Errorf("parse created router: %w (body=%s)", err, provisionSummarize(respBody))
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("created router has no id (body=%s)", provisionSummarize(respBody))
	}
	return created.ID, nil
}

// ensureKnowledgeFileRouter returns the ID of the org's knowledge_file
// router, creating one if missing.
func ensureKnowledgeFileRouter(c *provisionClient, orgID uint) (uint, error) {
	body, status, err := c.getForOrg("/routers/", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list routers", status, body, err)
	}
	var existing []routerListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return 0, fmt.Errorf("parse routers: %w (body=%s)", err, provisionSummarize(body))
	}
	var matches []routerListItem
	for _, r := range existing {
		if r.SourceType == "knowledge_file" {
			matches = append(matches, r)
		}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("id=%d name=%q", m.ID, m.Name))
		}
		return 0, fmt.Errorf("ambiguous knowledge_file routers in org %d (%d found: %s) — delete duplicates and re-run", orgID, len(matches), strings.Join(ids, ", "))
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}

	create := map[string]any{
		"name":          "Knowledge files (auto)",
		"source_type":   "knowledge_file",
		"source_config": "{}",
	}
	fmt.Printf("CREATE router source=knowledge_file org=%d (auto, holds all knowledge-file rules)\n", orgID)
	payload, _ := json.Marshal(create)
	respBody, status, err := c.writeForOrg("POST", "/routers/", payload, orgID)
	if err != nil || status >= 300 {
		return 0, fmt.Errorf("create router status=%d err=%v body=%s", status, err, provisionSummarize(respBody))
	}
	var created routerListItem
	if err := json.Unmarshal(respBody, &created); err != nil {
		return 0, fmt.Errorf("parse created router: %w (body=%s)", err, provisionSummarize(respBody))
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("created router has no id (body=%s)", provisionSummarize(respBody))
	}
	return created.ID, nil
}

// resolvePlaybookIDByName looks up a playbook by case-insensitive name
// within the given org.
func resolvePlaybookIDByName(c *provisionClient, orgID uint, name string) (uint, error) {
	body, status, err := c.getForOrg("/playbooks/", orgID)
	if err != nil || status != 200 {
		return 0, provisionAPIErr("list playbooks for dispatch resolve", status, body, err)
	}
	var existing []provisionPlaybookListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return 0, fmt.Errorf("parse playbooks: %w (body=%s)", err, provisionSummarize(body))
	}
	var matches []provisionPlaybookListItem
	for _, p := range existing {
		if strings.EqualFold(p.Name, name) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return 0, fmt.Errorf("dispatch playbook %q not found in org %d (install the playbook YAML first)", name, orgID)
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("%d", m.ID))
		}
		return 0, fmt.Errorf("dispatch playbook %q matches %d playbooks (%s) — rename to disambiguate", name, len(matches), strings.Join(ids, ","))
	}
	return matches[0].ID, nil
}

// listRouterRules returns rules for the given router.
func listRouterRules(c *provisionClient, routerID uint, orgID uint) ([]routerRuleRemote, error) {
	body, status, err := c.getForOrg(fmt.Sprintf("/routers/%d", routerID), orgID)
	if err != nil || status != 200 {
		return nil, provisionAPIErr("get router", status, body, err)
	}
	var envelope struct {
		Data routerListItem `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data.ID != 0 {
		return envelope.Data.Rules, nil
	}
	var r routerListItem
	if err2 := json.Unmarshal(body, &r); err2 != nil {
		return nil, fmt.Errorf("parse router (bare): %w (body=%s)", err2, provisionSummarize(body))
	}
	return r.Rules, nil
}

// buildKnowledgeFileMatchConfig converts YAML conditions to the JSON
// config blob shape consumed by internal/router/knowledge_file_evaluator.go.
func buildKnowledgeFileMatchConfig(conds []routerRuleCondition) ([]byte, error) {
	type jsonCond struct {
		Field string          `json:"field"`
		Op    string          `json:"op"`
		Value json.RawMessage `json:"value"`
	}
	type cfg struct {
		Conditions []jsonCond `json:"conditions"`
	}
	out := cfg{Conditions: make([]jsonCond, 0, len(conds))}
	for _, c := range conds {
		valueJSON, err := json.Marshal(c.Value)
		if err != nil {
			return nil, fmt.Errorf("marshal value for field=%s op=%s: %w", c.Field, c.Op, err)
		}
		out.Conditions = append(out.Conditions, jsonCond{
			Field: c.Field,
			Op:    c.Op,
			Value: valueJSON,
		})
	}
	return json.Marshal(out)
}
