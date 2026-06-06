// provision_credentials.go — provision support for OrgCredential resources.
//
// Mirrors playbook.go shape: list, match by name, upsert. Idempotent: re-running
// with the same YAML produces NOOPs after the first apply.
//
// Schema (studio/credentials/<name>.yaml):
//
//	name: slack-webhook-milestones
//	credential_type: webhook
//	description: Slack incoming webhook for #milestones channel
//	values:
//	  default_url:
//	    secret_env: SLACK_WEBHOOK_MILESTONES   # CI-friendly
//	  # OR
//	  default_url:
//	    secret_vault: taufinity/slack-webhook-milestones   # local: shells out to token-vault
//	  # Plain (non-secret) values:
//	  auth_header:
//	    value: Authorization
//
// Resolution order per field: value → secret_env → secret_vault. Exactly one
// must be set per field; provision fails loud if zero or multiple are set.
//
// Secrets never touch disk. They're read at apply time, marshalled into a
// JSON blob, and posted to the credentials API which encrypts at rest.
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// credentialConfig is the YAML shape for a single OrgCredential.
type credentialConfig struct {
	Name           string                         `yaml:"name"`
	CredentialType string                         `yaml:"credential_type"`
	Description    string                         `yaml:"description,omitempty"`
	Values         map[string]credentialValueSpec `yaml:"values"`
}

// credentialValueSpec describes how to obtain one field's value at apply time.
// Exactly one of {Value, SecretEnv, SecretVault} must be set.
//
// Value is `any` so non-string types (numbers, booleans) round-trip correctly
// into the credential's values_json. e.g. an Odoo `uid` of 6 must serialise as
// the JSON number 6 — quoting it as "6" in YAML breaks the runner's
// OdooReadConfig.UID int unmarshal. Secret-source fields stay string-typed
// because env vars and vault outputs are inherently string.
type credentialValueSpec struct {
	Value       any    `yaml:"value,omitempty"`
	SecretEnv   string `yaml:"secret_env,omitempty"`
	SecretVault string `yaml:"secret_vault,omitempty"`
}

// credentialListItem is the minimal shape we need from GET /api/credentials/.
type credentialListItem struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	CredentialType string `json:"credential_type"`
}

// vaultBinary is the executable path used to fetch from token-vault. Overridable
// via TOKEN_VAULT_BIN env var (used in tests). Default uses PATH lookup.
var vaultBinary = func() string {
	if v := os.Getenv("TOKEN_VAULT_BIN"); v != "" {
		return v
	}
	return "token-vault"
}

// upsertCredential creates or updates an OrgCredential for the given org.
// Match key: case-insensitive name. Refuses to apply if more than one
// credential with the same name exists in the org (would be ambiguous).
func upsertCredential(c *provisionClient, orgID uint, cfg credentialConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("credential: name is required")
	}
	if strings.TrimSpace(cfg.CredentialType) == "" {
		return fmt.Errorf("credential %q: credential_type is required", cfg.Name)
	}
	if len(cfg.Values) == 0 {
		return fmt.Errorf("credential %q: at least one value is required", cfg.Name)
	}

	// Resolve all values up front. Failing here means the API never sees a
	// partial credential, and we don't fire a half-baked write.
	resolved, err := resolveCredentialValues(cfg.Values)
	if err != nil {
		return fmt.Errorf("credential %q: %w", cfg.Name, err)
	}
	valuesJSON, err := json.Marshal(resolved)
	if err != nil {
		return fmt.Errorf("credential %q: marshal values: %w", cfg.Name, err)
	}

	body, status, err := c.getForOrg("/credentials/", orgID)
	if err != nil || status != 200 {
		return provisionAPIErr("list credentials", status, body, err)
	}
	var existing []credentialListItem
	if err := unmarshalListEnvelope(body, &existing); err != nil {
		return fmt.Errorf("parse credentials: %w (body=%s)", err, provisionSummarize(body))
	}
	var matches []credentialListItem
	for _, cred := range existing {
		if strings.EqualFold(cred.Name, cfg.Name) {
			matches = append(matches, cred)
		}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("id=%d", m.ID))
		}
		return fmt.Errorf("credential %q: %d matches in org %d (%s) — ambiguous, refusing to apply",
			cfg.Name, len(matches), orgID, strings.Join(ids, ", "))
	}

	if len(matches) == 1 {
		fmt.Printf("UPDATE credential %q id=%d type=%s\n", cfg.Name, matches[0].ID, cfg.CredentialType)
		payload, _ := json.Marshal(map[string]any{
			"name":        cfg.Name,
			"values_json": string(valuesJSON),
		})
		respBody, status, err := c.writeForOrg("PUT", fmt.Sprintf("/credentials/%d/", matches[0].ID), payload, orgID)
		if err != nil || status >= 300 {
			return provisionAPIErr(fmt.Sprintf("update credential %q", cfg.Name), status, respBody, err)
		}
		return nil
	}

	fmt.Printf("CREATE credential %q type=%s\n", cfg.Name, cfg.CredentialType)
	payload, _ := json.Marshal(map[string]any{
		"name":            cfg.Name,
		"credential_type": cfg.CredentialType,
		"values_json":     string(valuesJSON),
	})
	respBody, status, err := c.writeForOrg("POST", "/credentials/", payload, orgID)
	if err != nil || status >= 300 {
		return provisionAPIErr(fmt.Sprintf("create credential %q", cfg.Name), status, respBody, err)
	}
	return nil
}

// resolveCredentialValues turns the YAML value specs into a map ready for
// JSON-marshalling into the credential's values_json. Exactly one source
// must be set per field.
//
// Output value type follows the source: `value` keeps its native YAML type
// (string, int, bool, …) so a Go-side struct field with `int` accepts a
// number; secret_env and secret_vault always yield strings.
//
// Errors include the field name so users can spot which field is misconfigured
// without enabling debug output.
func resolveCredentialValues(specs map[string]credentialValueSpec) (map[string]any, error) {
	out := make(map[string]any, len(specs))
	for field, spec := range specs {
		hasValue := spec.Value != nil
		hasEnv := spec.SecretEnv != ""
		hasVault := spec.SecretVault != ""
		sources := 0
		if hasValue {
			sources++
		}
		if hasEnv {
			sources++
		}
		if hasVault {
			sources++
		}
		if sources == 0 {
			return nil, fmt.Errorf("field %q: must set one of value/secret_env/secret_vault", field)
		}
		if sources > 1 {
			return nil, fmt.Errorf("field %q: must set exactly one of value/secret_env/secret_vault, got %d", field, sources)
		}

		switch {
		case hasValue:
			interpolated, err := interpolateEnvVars(spec.Value)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", field, err)
			}
			out[field] = interpolated
		case hasEnv:
			v := os.Getenv(spec.SecretEnv)
			if v == "" {
				return nil, fmt.Errorf("field %q: env var %s is empty or unset", field, spec.SecretEnv)
			}
			out[field] = v
		case hasVault:
			v, err := readFromTokenVault(spec.SecretVault)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", field, err)
			}
			out[field] = v
		}
	}
	return out, nil
}

// interpolateEnvVars recursively substitutes ${VAR_NAME} patterns within v.
// Strings are expanded in place. Maps and slices are walked recursively.
// Non-string scalars (int, bool, float64) are returned unchanged.
// Returns an error if any referenced variable is empty or unset.
//
// This lets credential YAML files embed env var references inside nested
// `value` maps — useful when a single value key holds a map (e.g. `headers`):
//
//	headers:
//	  value:
//	    Authorization: "Bearer ${RESEND_API_KEY}"
func interpolateEnvVars(v any) (any, error) {
	switch val := v.(type) {
	case string:
		return expandEnvVarsInString(val)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			resolved, err := interpolateEnvVars(elem)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			resolved, err := interpolateEnvVars(elem)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return v, nil
	}
}

// expandEnvVarsInString replaces ${VAR_NAME} tokens with their values from
// os.Getenv. Returns an error for any variable that is empty or unset so that
// provision fails loudly rather than writing a partial credential.
func expandEnvVarsInString(s string) (string, error) {
	var missing string
	result := os.Expand(s, func(key string) string {
		val := os.Getenv(key)
		if val == "" && missing == "" {
			missing = key
		}
		return val
	})
	if missing != "" {
		return "", fmt.Errorf("env var ${%s} is empty or unset — fix with: export %s=<value>", missing, missing)
	}
	return result, nil
}

// readFromTokenVault shells out to `token-vault get $customer $name`. The vault
// must already be unlocked (interactive passphrase entry happens out of band).
//
// Spec format: "customer/secret-name". Both halves required.
func readFromTokenVault(spec string) (string, error) {
	customer, name, ok := strings.Cut(spec, "/")
	if !ok || customer == "" || name == "" {
		return "", fmt.Errorf("secret_vault %q: expected format 'customer/secret-name'", spec)
	}
	cmd := exec.Command(vaultBinary(), "get", customer, name)
	cmd.Stderr = os.Stderr // surface vault errors (e.g. locked vault) to the operator
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token-vault get %s/%s: %w (is the vault unlocked?)", customer, name, err)
	}
	value := strings.TrimRight(string(out), "\n\r")
	if value == "" {
		return "", fmt.Errorf("token-vault get %s/%s: empty value", customer, name)
	}
	return value, nil
}

// unmarshalListEnvelope handles the three list-envelope shapes the Studio
// API returns: bare array, wrapped object with a known key, or single-key object.
func unmarshalListEnvelope(body []byte, out interface{}) error {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		return json.Unmarshal(body, out)
	}
	if strings.HasPrefix(trimmed, "{") {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			return err
		}
		// Common envelopes: data, items, definitions, playbooks, widgets, etc.
		for _, k := range []string{"data", "items", "definitions", "playbooks", "widgets", "files"} {
			if raw, ok := top[k]; ok {
				return json.Unmarshal(raw, out)
			}
		}
		// Single top-level key whose value is an array — accept it.
		for _, raw := range top {
			trim := strings.TrimSpace(string(raw))
			if strings.HasPrefix(trim, "[") {
				return json.Unmarshal(raw, out)
			}
		}
		// Empty/unknown object — treat as empty list.
		return nil
	}
	return fmt.Errorf("unexpected response shape: %s", provisionSummarize(body))
}

func applyCredentials(c *provisionClient, dir string, orgID uint) error {
	cd := filepath.Join(dir, "credentials")
	if !fileExists(cd) {
		return nil
	}
	entries, err := os.ReadDir(cd)
	if err != nil {
		return fmt.Errorf("credentials/: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		cf := filepath.Join(cd, e.Name())
		var cfg credentialConfig
		mustReadYAML(cf, &cfg)
		if err := upsertCredential(c, orgID, cfg); err != nil {
			return fmt.Errorf("credential %s: %w", e.Name(), err)
		}
	}
	return nil
}
