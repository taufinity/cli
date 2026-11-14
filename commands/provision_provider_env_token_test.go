package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const envTokenSpec = `id: 25
name: WVS BQ Tracking
provider_type: bigquery
category: tracking
endpoint_url: "content-gen-484211.feedback_{env}"
enabled: true
allowed_tables:
  - quote_line_items
`

// endpointSentFor applies a provider spec against a fake Studio and returns the
// endpoint_url that provision actually sent, plus the spec file as it was left
// on disk. The second return value is the point of the token: the committed
// spec has to stay environment-independent.
func endpointSentFor(t *testing.T, spec, apiURLOverride string) (string, string, error) {
	t.Helper()
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/custom-ai-providers":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 25, "name": "WVS BQ Tracking"},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/custom-ai-providers/25":
			body, _ := io.ReadAll(r.Body)
			var put map[string]interface{}
			if err := json.Unmarshal(body, &put); err != nil {
				t.Errorf("PUT body is not JSON: %v", err)
			}
			if v, ok := put["endpoint_url"].(string); ok {
				sent = v
			}
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/admin/bq-providers/"):
			// A bigquery provider with allowed_tables writes its table
			// boundary through a second call; the token has nothing to do
			// with it, but the apply fails without it.
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	pd := filepath.Join(dir, "providers")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(pd, "p.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	base := srv.URL
	if apiURLOverride != "" {
		base = apiURLOverride
	}
	c := newProvisionClient(base, "key", false)
	_, _, err := applyProviders(c, dir, filepath.Dir(dir), 1)
	onDisk, readErr := os.ReadFile(specPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return sent, string(onDisk), err
}

// withEnvFlag sets the --env flag for one test and restores it after. flagEnv
// is package state, so leaking it would make neighbouring tests depend on
// execution order.
func withEnvFlag(t *testing.T, v string) {
	t.Helper()
	prev := flagEnv
	flagEnv = v
	t.Cleanup(func() { flagEnv = prev })
	prevEnv, had := os.LookupEnv("TAUFINITY_ENV")
	os.Unsetenv("TAUFINITY_ENV")
	t.Cleanup(func() {
		if had {
			os.Setenv("TAUFINITY_ENV", prevEnv)
		} else {
			os.Unsetenv("TAUFINITY_ENV")
		}
	})
}

func TestProviderEnvToken_StagingResolvesToStaging(t *testing.T) {
	withEnvFlag(t, "staging")
	sent, onDisk, err := endpointSentFor(t, envTokenSpec, "")
	if err != nil {
		t.Fatalf("applyProviders: %v", err)
	}
	if want := "content-gen-484211.feedback_staging"; sent != want {
		t.Errorf("endpoint sent = %q, want %q", sent, want)
	}
	// The whole point: staging must not write into production's dataset.
	if strings.Contains(sent, "feedback_prod") {
		t.Errorf("staging resolved to production's dataset: %q", sent)
	}
	if !strings.Contains(onDisk, "{env}") {
		t.Errorf("the spec on disk must keep the token so it stays environment-independent, got:\n%s", onDisk)
	}
}

func TestProviderEnvToken_ProductionResolvesToProd(t *testing.T) {
	withEnvFlag(t, "production")
	sent, _, err := endpointSentFor(t, envTokenSpec, "")
	if err != nil {
		t.Fatalf("applyProviders: %v", err)
	}
	if want := "content-gen-484211.feedback_prod"; sent != want {
		t.Errorf("endpoint sent = %q, want %q", sent, want)
	}
}

// With no flag set, the host decides. --api-url outranks --env when the host is
// picked, so a spec resolving from the flag alone could resolve to prod while
// the writes went to staging.
func TestProviderEnvToken_NoFlagFallsBackToTheHost(t *testing.T) {
	for _, tc := range []struct{ apiURL, want string }{
		{StagingAPIURL, "content-gen-484211.feedback_staging"},
		{DefaultAPIURL, "content-gen-484211.feedback_prod"},
		{DefaultAPIURL + "/", "content-gen-484211.feedback_prod"},
	} {
		t.Run(tc.apiURL, func(t *testing.T) {
			withEnvFlag(t, "")
			// The client points at the real host, so no PUT is observed; the
			// resolution is what this asserts, via the error-free path and the
			// resolved value the helper reports.
			got, err := provisionEnvName(tc.apiURL)
			if err != nil {
				t.Fatalf("provisionEnvName(%q): %v", tc.apiURL, err)
			}
			resolved := strings.ReplaceAll("content-gen-484211.feedback_{env}", envToken, got)
			if resolved != tc.want {
				t.Errorf("resolved %q, want %q", resolved, tc.want)
			}
		})
	}
}

// An unknown host is refused, not guessed. Guessing prod on a custom host is
// how a spec silently writes to production again.
func TestProviderEnvToken_UnknownHostIsRefused(t *testing.T) {
	withEnvFlag(t, "")
	_, _, err := endpointSentFor(t, envTokenSpec, "http://127.0.0.1:9999")
	if err == nil {
		t.Fatal("an unknown API URL must be refused when the spec uses the token")
	}
	for _, want := range []string{"{env}", "--env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q so the operator knows what to do; got: %v", want, err)
		}
	}
}

func TestProviderEnvToken_UnknownEnvValueIsRefused(t *testing.T) {
	withEnvFlag(t, "acceptatie")
	_, _, err := endpointSentFor(t, envTokenSpec, "")
	if err == nil {
		t.Fatal("an unrecognised --env value must be refused when the spec uses the token")
	}
	if !strings.Contains(err.Error(), "acceptatie") {
		t.Errorf("error must quote the offending value; got: %v", err)
	}
}

// A spec without the token is untouched, including on a host that would
// otherwise be refused. The token is opt-in and must not make existing specs
// fail.
func TestProviderEnvToken_SpecWithoutTokenIsUntouched(t *testing.T) {
	withEnvFlag(t, "")
	literal := strings.Replace(envTokenSpec, "feedback_{env}", "feedback_prod", 1)
	sent, _, err := endpointSentFor(t, literal, "")
	if err != nil {
		t.Fatalf("a spec without the token must apply unchanged: %v", err)
	}
	if want := "content-gen-484211.feedback_prod"; sent != want {
		t.Errorf("endpoint sent = %q, want %q", sent, want)
	}
	// And an unknown host must not refuse it either: without the token there is
	// nothing to resolve, so the environment never has to be known. Checked
	// directly rather than through an apply, so an unreachable host cannot make
	// this pass for the wrong reason.
	cfg := providerConfig{Name: "WVS BQ Tracking", EndpointURL: "content-gen-484211.feedback_prod"}
	if err := resolveProviderEnvToken(&cfg, "http://unknown.invalid"); err != nil {
		t.Errorf("a spec without the token must not be refused on an unknown host: %v", err)
	}
	if cfg.EndpointURL != "content-gen-484211.feedback_prod" {
		t.Errorf("endpoint_url must be untouched, got %q", cfg.EndpointURL)
	}
}
