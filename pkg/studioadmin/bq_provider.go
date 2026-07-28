package studioadmin

import (
	"encoding/json"
	"fmt"
)

// BQProvider is a Studio BigQuery data provider. `allowed_tables` is the access
// boundary — the exact set of tables the provider (and thus query_insights /
// dashboards) may read. The admin API stores it as a JSON-array STRING; this type
// exposes it as a slice and handles the round-trip.
type BQProvider struct {
	ID            int64
	Name          string
	Description   string
	EndpointURL   string
	AllowedTables []string
	Enabled       bool
}

// bqProviderAPI mirrors the admin API JSON: allowed_tables is a stringified array,
// and enabled is `is_enabled`.
type bqProviderAPI struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	EndpointURL   string `json:"endpoint_url"`
	AllowedTables string `json:"allowed_tables"` // JSON array string, e.g. "[\"t1\",\"t2\"]"
	Enabled       bool   `json:"is_enabled"`
}

// GetBQProvider reads a provider by id (GET /api/admin/bq-providers/{id}) and
// decodes the stringified allowed_tables into a slice.
func (c *Client) GetBQProvider(id int64) (*BQProvider, error) {
	var raw bqProviderAPI
	if err := c.Get(fmt.Sprintf("/admin/bq-providers/%d", id), &raw); err != nil {
		return nil, err
	}
	tables := []string{}
	if raw.AllowedTables != "" {
		if err := json.Unmarshal([]byte(raw.AllowedTables), &tables); err != nil {
			return nil, fmt.Errorf("studioadmin: decode allowed_tables %q: %w", raw.AllowedTables, err)
		}
	}
	return &BQProvider{
		ID:            raw.ID,
		Name:          raw.Name,
		Description:   raw.Description,
		EndpointURL:   raw.EndpointURL,
		AllowedTables: tables,
		Enabled:       raw.Enabled,
	}, nil
}

// UpdateBQProvider writes the mutable fields (PUT /api/admin/bq-providers/{id}),
// encoding allowed_tables back into the JSON-array string the API expects.
func (c *Client) UpdateBQProvider(p *BQProvider) error {
	tablesJSON, err := json.Marshal(p.AllowedTables)
	if err != nil {
		return fmt.Errorf("studioadmin: encode allowed_tables: %w", err)
	}
	enabled := p.Enabled
	body := map[string]any{
		"name":           p.Name,
		"description":    p.Description,
		"endpoint_url":   p.EndpointURL,
		"allowed_tables": string(tablesJSON),
		"is_enabled":     &enabled,
	}
	return c.Write("PUT", fmt.Sprintf("/admin/bq-providers/%d", p.ID), body, nil, nil)
}
