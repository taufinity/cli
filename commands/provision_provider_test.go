package commands

import "testing"

func TestValidateProviderBoundary_AllowedTablesPresence(t *testing.T) {
	t.Run("missing is rejected", func(t *testing.T) {
		cfg := providerConfig{Name: "analytics", ProviderType: "bigquery"}
		if err := validateProviderBoundary(cfg); err == nil {
			t.Fatal("missing allowed_tables must be rejected")
		}
	})

	t.Run("explicit empty list is accepted", func(t *testing.T) {
		var cfg providerConfig
		if err := yamlUnmarshalStrict([]byte("name: analytics\nprovider_type: bigquery\nallowed_tables: []\n"), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.AllowedTables == nil {
			t.Fatal("explicit allowed_tables: [] must remain distinguishable from an omitted field")
		}
		if err := validateProviderBoundary(cfg); err != nil {
			t.Fatalf("explicit empty allowed_tables must be accepted: %v", err)
		}
	})

	t.Run("not required for REST providers", func(t *testing.T) {
		cfg := providerConfig{Name: "api", ProviderType: "rest"}
		if err := validateProviderBoundary(cfg); err != nil {
			t.Fatalf("REST provider: %v", err)
		}
	})
}
