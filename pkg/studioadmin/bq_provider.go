package studioadmin

import (
	"encoding/json"
	"fmt"
)

// BQProvider is a Studio BigQuery data provider. `AllowedTables` is the access
// boundary — the exact set of tables the provider (and thus query_insights /
// dashboards) may read. The admin API stores it as a JSON-array STRING; this type
// exposes it as a slice and handles the round-trip.
//
// A BQ provider is COMPOSITE: the base record is a `custom-ai-provider`
// (create/delete/base-update), while `allowed_tables` is persisted via the separate
// `admin/bq-providers/{id}` PUT (the base PUT deliberately ignores allowed_tables).
type BQProvider struct {
	ID             int64
	Name           string
	Description    string
	Category       string // e.g. data_enrichment
	EndpointURL    string // project.dataset
	HTTPMethod     string // e.g. GET
	AllowedTables  []string
	MaxBytesBilled int64
	Enabled        bool
}

const bqProviderType = "bigquery"

type bqProviderAPI struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Category      string `json:"category"`
	EndpointURL   string `json:"endpoint_url"`
	HTTPMethod    string `json:"http_method"`
	AllowedTables string `json:"allowed_tables"` // JSON array string
	MaxBytes      int64  `json:"max_bytes_billed"`
	Enabled       bool   `json:"is_enabled"`
}

// GetBQProvider reads a provider by id (GET /admin/bq-providers/{id}) and decodes
// the stringified allowed_tables into a slice.
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
		ID:             raw.ID,
		Name:           raw.Name,
		Description:    raw.Description,
		Category:       raw.Category,
		EndpointURL:    raw.EndpointURL,
		HTTPMethod:     raw.HTTPMethod,
		AllowedTables:  tables,
		MaxBytesBilled: raw.MaxBytes,
		Enabled:        raw.Enabled,
	}, nil
}

// basePayload is the custom-ai-provider record fields (everything except the
// allowed_tables, which is written separately).
func (p *BQProvider) basePayload() map[string]any {
	return map[string]any{
		"name":             p.Name,
		"description":      p.Description,
		"provider_type":    bqProviderType,
		"category":         p.Category,
		"endpoint_url":     p.EndpointURL,
		"http_method":      p.HTTPMethod,
		"max_bytes_billed": p.MaxBytesBilled,
	}
}

// writeAllowedTables persists allowed_tables via the admin/bq-providers endpoint
// (the base custom-ai-provider PUT ignores it).
func (c *Client) writeAllowedTables(p *BQProvider) error {
	tablesJSON, err := json.Marshal(p.AllowedTables)
	if err != nil {
		return fmt.Errorf("studioadmin: encode allowed_tables: %w", err)
	}
	return c.Write("PUT", fmt.Sprintf("/admin/bq-providers/%d", p.ID),
		map[string]any{"allowed_tables": string(tablesJSON)}, nil, nil)
}

// CreateBQProvider creates the composite provider: POST the base record, then write
// allowed_tables. Returns the new id.
func (c *Client) CreateBQProvider(p *BQProvider) (int64, error) {
	var created struct {
		ID int64 `json:"id"`
	}
	if err := c.Write("POST", "/custom-ai-providers", p.basePayload(), &created, nil); err != nil {
		return 0, err
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("studioadmin: create returned no id")
	}
	p.ID = created.ID
	if err := c.writeAllowedTables(p); err != nil {
		return created.ID, fmt.Errorf("studioadmin: created id=%d but allowed_tables write failed: %w", created.ID, err)
	}
	return created.ID, nil
}

// UpdateBQProvider updates the base record and re-writes allowed_tables.
func (c *Client) UpdateBQProvider(p *BQProvider) error {
	if err := c.Write("PUT", fmt.Sprintf("/custom-ai-providers/%d", p.ID), p.basePayload(), nil, nil); err != nil {
		return err
	}
	return c.writeAllowedTables(p)
}

// DeleteBQProvider deletes the composite provider (DELETE cascades from the base record).
func (c *Client) DeleteBQProvider(id int64) error {
	return c.Write("DELETE", fmt.Sprintf("/custom-ai-providers/%d", id), nil, nil, nil)
}
