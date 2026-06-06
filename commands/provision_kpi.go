package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

type kpiEntry struct {
	KPISlug string `yaml:"kpi_slug" json:"kpi_slug"`
}

type kpiClientConfig struct {
	ClientID string     `yaml:"client_id"`
	KPIs     []kpiEntry `yaml:"kpis"`
}

// upsertKPIConfig calls PUT /api/kpis/config once per KPI entry.
// The endpoint is per-row (client_id + kpi_slug), not bulk.
func upsertKPIConfig(c *provisionClient, cfg kpiClientConfig) error {
	if len(cfg.KPIs) == 0 {
		return nil
	}
	fmt.Printf("provision: upserting %d KPI configs for client %q\n", len(cfg.KPIs), cfg.ClientID)
	for _, kpi := range cfg.KPIs {
		payload, _ := json.Marshal(map[string]interface{}{
			"client_id": cfg.ClientID,
			"kpi_slug":  kpi.KPISlug,
		})
		_, status, err := c.put("/kpis/config", payload)
		if err != nil || status >= 300 {
			return fmt.Errorf("upsert KPI %q for client %q: status=%d err=%v",
				kpi.KPISlug, cfg.ClientID, status, err)
		}
	}
	return nil
}

func applyKPI(c *provisionClient, dir string, orgID uint) error {
	kf := filepath.Join(dir, "kpis", "client-config.yaml")
	if !fileExists(kf) {
		return nil
	}
	var cfg kpiClientConfig
	mustReadYAML(kf, &cfg)
	return upsertKPIConfig(c, cfg)
}
