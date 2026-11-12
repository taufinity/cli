# Independent review: review command

Reviewer: independent subagent, not the author
Verdict: request_changes

### F1: Unquoted secrets, URL credentials and Bearer headers are not masked
Severity: high
Location: internal/review/mask.go:28
Description: Name-based masking only matched quoted values. `DB_PASSWORD=hunter2hunter2`, `password: hunter2hunter2`, `export API_TOKEN=...`, a URL carrying `user:password@host` and `Authorization: Bearer ...` came back unchanged, so shell, ini, properties and unquoted YAML secrets would be sent in clear text.
Disposition: fixed (TestMaskUnquotedAndURLCredentials)

### F2: API URL is not restricted to a Studio host
Severity: medium
Location: commands/review.go:322, commands/root.go:220-244
Description: requireSecureStudioURL only requires https (or http to localhost); `--api-url`, `TAUFINITY_API_URL` and the config `api_url` can point at any https host, which contradicts the README claim that reviews go nowhere else.
Disposition: accepted (the URL is the user's own explicit configuration, with the same trust as the token for every command; the README claim is reworded to say so)

### F3: Debug output of existing commands now redacts the Bearer prefix
Severity: low
Location: internal/api/client.go:161
Description: The 12-character Bearer token prefix in debug output became `Bearer (redacted)`, which goes beyond the review command.
Disposition: accepted (intentional security improvement that only removes secret material from debug output)

### F4: install-hook --force writes through a symlink
Severity: low
Location: commands/review_hook.go:968-974
Description: An existing pre-push symlink was followed by WriteFile, overwriting a target that may live outside .git/hooks.
Disposition: fixed (TestInstallHookForceReplacesSymlink)

### F5: .env and id_rsa exclusion checks are case-sensitive
Severity: low
Location: internal/review/exclude.go:50
Description: `.ENV` and similar names were not excluded while suffix checks were case-insensitive.
Disposition: fixed (TestExcludedPathCaseInsensitiveEnv)
