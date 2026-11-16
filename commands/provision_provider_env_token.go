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
	"net/url"
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
// The host the client is actually pointed at decides whenever it is one of the
// two known hosts, because that is where the writes go. --env (or
// TAUFINITY_ENV) does not pick the host on its own: resolveAPIURL lets
// --api-url, TAUFINITY_API_URL and config api_url outrank it. So a flag that
// contradicts a known host is refused, not obeyed: obeying it resolved
// `--env staging --api-url https://studio.taufinity.io` to the staging dataset
// while every write went to production, which is the exact failure this token
// exists to prevent.
//
// The flag only decides for a host that is neither known one (a local server,
// a preview deployment), which genuinely does not say which dataset it belongs
// to. Without a flag such a host is refused rather than guessed: guessing prod
// there is how the original bug would come back.
func provisionEnvName(apiURL string) (string, error) {
	envFlag := flagEnv
	if envFlag == "" {
		envFlag = os.Getenv("TAUFINITY_ENV")
	}
	var fromFlag string
	switch strings.ToLower(strings.TrimSpace(envFlag)) {
	case envNameStaging:
		fromFlag = envNameStaging
	case "production", "prod":
		fromFlag = envNameProduction
	case "":
	default:
		return "", fmt.Errorf("cannot resolve %s: unknown environment %q "+
			"(expected \"staging\" or \"production\")", envToken, envFlag)
	}

	fromHost := knownHostEnv(apiURL)
	switch {
	case fromHost != "" && fromFlag != "" && fromHost != fromFlag:
		return "", fmt.Errorf("cannot resolve %s: environment %q says %s, but the API URL %q "+
			"is the %s host, which is where the writes go. Refusing to point this spec at "+
			"one environment's data while writing to the other: drop --env/TAUFINITY_ENV "+
			"or point --api-url at the matching host",
			envToken, envFlag, fromFlag, apiURL, fromHost)
	case fromHost != "":
		return fromHost, nil
	case fromFlag != "":
		return fromFlag, nil
	}
	return "", fmt.Errorf("cannot resolve %s: --api-url %q is neither the production "+
		"nor the staging host, so the environment is unknown. Pass --env staging or "+
		"--env production to say which dataset this spec means",
		envToken, apiURL)
}

// knownHostEnv names the environment of apiURL when its host is the production
// or the staging Studio host, and returns "" for any other host. It compares
// host names, not whole URLs, so a trailing slash, a path or a different scheme
// cannot turn a known host into an "unknown" one that the flag would decide.
func knownHostEnv(apiURL string) string {
	u, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case hostOf(StagingAPIURL):
		return envNameStaging
	case hostOf(DefaultAPIURL):
		return envNameProduction
	}
	return ""
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
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
