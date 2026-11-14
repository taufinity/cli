// provision_provider_env_token.go — the {env} token in a provider's endpoint_url.
//
// Why this exists. WVS's provider spec carried a literal
// "content-gen-484211.feedback_prod", so every environment that applied that
// spec wrote its quote analytics into production's dataset. Staging's quote
// runs landed in production's tables, and nothing in the spec said so: the file
// read as if it described one environment, and it was applied to two.
//
// A spec can now write "content-gen-484211.feedback_{env}" and have it resolve
// per environment. The token is deliberately only honoured in endpoint_url:
// other provider fields hold JSON templates full of braces
// (request_body_template, response_mappings), and substituting across those
// would mangle a payload to fix a dataset name.
package commands

import (
	"fmt"
	"os"
	"strings"
)

// envToken is what a spec writes where the environment name belongs.
const envToken = "{env}"

// Environment names a {env} token resolves to. These are the dataset suffixes
// in use, not the CLI's flag values: --env staging resolves to "staging" and
// the production default resolves to "prod".
const (
	envNameStaging    = "staging"
	envNameProduction = "prod"
)

// provisionEnvName reports which environment a {env} token resolves to.
//
// The flag is the primary signal, because it is the same one resolveAPIURL
// reads when it picks the host. When no flag is set, the API URL the client is
// actually pointed at decides, so a spec cannot resolve to prod while the
// writes go to staging — which is the exact failure this token exists to
// prevent, and --api-url outranks --env when the host is chosen.
//
// An API URL that is neither known host is refused rather than guessed. A
// custom host (a local server, a preview deployment) genuinely does not say
// which dataset it belongs to, and guessing prod there is how the original bug
// would come back.
func provisionEnvName(apiURL string) (string, error) {
	envFlag := flagEnv
	if envFlag == "" {
		envFlag = os.Getenv("TAUFINITY_ENV")
	}
	switch strings.ToLower(strings.TrimSpace(envFlag)) {
	case envNameStaging:
		return envNameStaging, nil
	case "production", "prod":
		return envNameProduction, nil
	case "":
		// Fall through to the host.
	default:
		return "", fmt.Errorf("cannot resolve %s: unknown environment %q "+
			"(expected \"staging\" or \"production\")", envToken, envFlag)
	}

	switch strings.TrimRight(apiURL, "/") {
	case StagingAPIURL:
		return envNameStaging, nil
	case DefaultAPIURL:
		return envNameProduction, nil
	}
	return "", fmt.Errorf("cannot resolve %s: --api-url %q is neither the production "+
		"nor the staging host, so the environment is unknown. Pass --env staging or "+
		"--env production to say which dataset this spec means",
		envToken, apiURL)
}

// resolveProviderEnvToken replaces {env} in cfg.EndpointURL. The spec file on
// disk keeps the token: only the struct that is about to be sent is resolved,
// so the committed spec stays environment-independent.
func resolveProviderEnvToken(cfg *providerConfig, apiURL string) error {
	if !strings.Contains(cfg.EndpointURL, envToken) {
		return nil
	}
	env, err := provisionEnvName(apiURL)
	if err != nil {
		return fmt.Errorf("provider %q endpoint_url %q: %w", cfg.Name, cfg.EndpointURL, err)
	}
	resolved := strings.ReplaceAll(cfg.EndpointURL, envToken, env)
	fmt.Printf("provision: provider %q endpoint_url %s -> %s\n", cfg.Name, cfg.EndpointURL, resolved)
	cfg.EndpointURL = resolved
	return nil
}
