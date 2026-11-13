# Taufinity CLI

Command-line tool for the [Taufinity](https://studio.taufinity.io) content platform. Manage templates, trigger playbooks, and configure Claude Code / Claude Desktop MCP integration.

## Installation

### macOS (Homebrew — recommended)

```bash
brew install --cask taufinity/tap/taufinity
```

Installs the `taufinity` binary and a daily background update-check that sends a macOS notification when a new version is available.

Alternatively, download `taufinity_darwin_installer.pkg` from [GitHub Releases](https://github.com/taufinity/cli/releases) and double-click to install.

### macOS / Linux (Go 1.21+)

```bash
go install github.com/taufinity/cli/cmd/taufinity@latest
```

The binary is installed as `taufinity` in `$GOPATH/bin` (usually `~/go/bin`). Make sure that directory is in your `$PATH`.

**zsh** (macOS default):
```bash
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

**bash**:
```bash
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.bashrc && source ~/.bashrc
```

Not sure which shell you're using? Run `echo $SHELL`.

### Verify installation

```bash
taufinity version
```

### Update

Use the built-in update command:

```bash
taufinity update           # install latest, with backup + smoke test
taufinity update --check   # report current vs latest, no install
taufinity update --rollback  # restore the previous binary
```

`taufinity update` backs up the running binary to `<path>.prev` before installing (when the install target is the same directory as the running binary), runs `go install github.com/taufinity/cli/cmd/taufinity@latest`, then smoke-tests the new binary. If the smoke test fails, the previous binary is restored automatically. Use `--rollback` if a working but unwanted version slipped in.

#### Staleness warning

On every invocation, taufinity makes a background call to GitHub (cached for 24h) and prints a one-line stderr warning when a newer commit is available on `main`. The check never blocks the command. Suppress it with:

```bash
TAUFINITY_NO_UPDATE_CHECK=1 taufinity ...       # one-off
taufinity config set update_check false         # permanent
```

#### Opt-in automatic updates

To have taufinity update itself automatically whenever a newer version is available, opt in once:

```bash
taufinity config set auto_update true
```

After each invocation where a newer version is detected, taufinity runs `taufinity update` in the foreground before exiting. The same backup + smoke-test + auto-rollback logic applies: the running binary is backed up, the new binary is smoke-tested, and if the test fails the previous binary is restored automatically. Users are never left with a broken binary.

To opt back out:

```bash
taufinity config set auto_update false
```

#### Security note

`taufinity update` installs from the `main` branch via `go install ...@latest`. Anyone with commit access to `main` ships to all CLI users on their next update. Acceptable for the small internal team today; once we cut tagged releases, the default will move to a tagged version.

### macOS background update agent

The `.pkg` installer (Homebrew or direct download) also installs a launchd agent that runs daily at 09:00 and fires a macOS notification when a newer version is available.

**Check the agent is running:**

```bash
launchctl list | grep io.taufinity
# -  0  io.taufinity.cli   ← loaded, last run succeeded
```

**Trigger the check manually:**

```bash
/usr/local/bin/taufinity-update-check
```

**View the agent log:**

```bash
cat /tmp/io.taufinity.cli.log
# [2026-07-03 09:00:01] Up to date
# [2026-07-03 09:00:01] Update available — firing notification
```

**Click-to-update notifications** (optional): install [`terminal-notifier`](https://github.com/julienXX/terminal-notifier) for actionable notifications with a button that opens Terminal and runs `taufinity update` automatically:

```bash
brew install terminal-notifier
```

The agent detects it automatically — no config needed. Falls back to a plain macOS notification when not installed.

**Uninstall the agent** (Homebrew handles this automatically on `brew uninstall`):

```bash
brew uninstall --cask taufinity
```

Or manually:

```bash
launchctl bootout "gui/$(id -u)/io.taufinity.cli"
sudo rm /Library/LaunchAgents/io.taufinity.cli.plist
sudo rm /usr/local/bin/taufinity /usr/local/bin/taufinity-update-check
```

### Build from source

```bash
git clone https://github.com/taufinity/cli.git
cd cli
make install    # installs to ~/bin/taufinity
```

To update: `git pull && make install`.

## Authentication

```bash
taufinity auth login
```

Opens a browser window for device authorization. Credentials are stored per server in `~/.config/taufinity/`: `credentials.json` for production, `credentials-<host>.json` for any other server (for example `credentials-staging-studio.taufinity.io.json` after `taufinity --env staging auth login`). Logging in to staging or a local server therefore never replaces or logs out your production session. A login that fails or is abandoned keeps the existing session.

Renewal is safe with many `taufinity` processes running at once (MCP bridges in every editor session, scripts calling `auth token`): one process renews under a file lock and the others pick up its result, so the rotating refresh token is never sent twice.

The access token is renewed before it expires, and also when the server refuses it early (HTTP 401), for example after Studio rotates its signing key: the CLI renews the session once from the refresh token and retries the request once. `auth status`, `auth token` and the `mcp stdio` bridge do the same, so a key rotation does not log anyone out. Only a rejected refresh token, or none stored, asks for `taufinity auth login`. A token written into a client config as a fixed header (`mcp install --transport http`, `mcp print`) cannot renew itself; use the stdio bridge for long-lived sessions.

### Staging

```bash
taufinity --env staging auth login      # or TAUFINITY_ENV=staging
taufinity --env staging auth status
```

`--env staging` targets `https://staging-studio.taufinity.io`, which sits behind Cloudflare Access. Every request needs the Access service token, and the CLI supplies it on its own, so no shell setup is needed in any terminal, IDE, MCP server or agent:

1. If `CF_ACCESS_CLIENT_ID` and `CF_ACCESS_CLIENT_SECRET` are both set, those are used. This is an explicit override, and like the other sources it only applies to the staging host.
2. Otherwise, for staging only, the token cached in `~/.config/taufinity/cf-access-staging-studio.taufinity.io.json` (mode 0600, next to `credentials.json`).
3. If there is no cache yet, the CLI reads `sitegen-cf-access-client-id` and `sitegen-cf-access-client-secret` from Secret Manager (project `content-gen-484211`) with your own `gcloud` login, with a 10-second limit, and writes the cache. After that, gcloud is not needed again until the token is rotated.

When Access refuses a cached token (it was rotated), the CLI deletes the cache, reads the token from gcloud again and retries the request once. The token is never logged, printed or sent with telemetry, and never sent to a host other than staging, not even when you set the variables yourself. Production never gets a token and never triggers gcloud.

`taufinity --env staging auth status` shows whether a token is in use and where it came from (environment, cache or gcloud), never the value.

If no token can be had you get one clear error instead of a JSON parse failure, for example:

```
staging-studio.taufinity.io is behind Cloudflare Access and no service token was available: gcloud is not logged in (run: gcloud auth login): .... Fix: gcloud auth login, or set CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET.
```

If a token was sent and still refused after the re-read, the error says the Access service token was refused (rotated or revoked?). Any other HTML page where the API should answer JSON (a proxy error page, a login wall) is reported as `expected JSON from <url>, got text/html (HTTP <status>)` with the page title, never the whole page.

Every path goes through the same logic: regular commands, `auth login` / token refresh / `auth revoke`, `provision`, `dashboards` and the `mcp stdio` bridge.

## Quick Start

```bash
# 1. Authenticate
taufinity auth login

# 2. Preview a template locally
taufinity template preview

# 3. Trigger a playbook
taufinity playbook trigger <playbook-id>
```

## Commands

| Command | Description |
|---------|-------------|
| `auth login` | Authenticate via browser |
| `auth status` | Check authentication status |
| `auth token` | Print current access token |
| `auth revoke` | Log out |
| `config set KEY VALUE` | Set a config value |
| `config get KEY` | Get a config value |
| `config list` | List all config values |
| `template preview` | Upload and preview a template locally |
| `template help-syntax` | Show template syntax reference |
| `playbook trigger <id>` | Trigger a playbook run |
| `playbook list` | List available playbooks |
| `playbook runs <id>` | Show recent runs |
| `org list` | List organizations |
| `mcp login` | Write credentials to project `.mcp.json` for Claude Code |
| `mcp install` | Add Taufinity Studio to one or more clients (`--client`) |
| `mcp uninstall` | Remove Taufinity Studio from one or more clients (`--client`) |
| `mcp print` | Print the server JSON block to stdout |
| `mcp stdio` | Run a stdio MCP bridge to Studio's `/mcp` endpoint (for stdio-only clients) |
| `update [--check\|--rollback]` | Update taufinity to the latest version (or check/rollback) |
| `version` | Print version info |

### One-command install across MCP clients

After `taufinity auth login`, install Taufinity Studio's MCP server into any supported client with a single command. By default the stdio bridge shape is written (no bearer token in the config — the bridge subprocess reads `~/.config/taufinity/credentials.json` fresh on every request).

```bash
taufinity mcp install                      # claude-desktop (default)
taufinity mcp install --client claude-code # ~/.claude.json
taufinity mcp install --client cursor      # ~/.cursor/mcp.json
taufinity mcp install --client vscode      # VS Code 1.95+ native MCP
taufinity mcp install --client antigravity # Google's Antigravity IDE
taufinity mcp install --client all         # every detected client
taufinity mcp install --client print       # preview the JSON, write nothing
```

**Detected** = the client's config directory already exists on disk (which only happens after you've launched the app once). `--client all` skips undetected clients with a summary line and exits 0 if at least one succeeded.

**Per-customer pinning** — pass `--org` (numeric ID or slug) and a custom `--label` to write a customer-scoped entry. The `--org` value is embedded into the bridge subprocess args, so every launch from that client uses the right organisation regardless of the user's global CLI config:

```bash
taufinity --org 3 mcp install --client all --label taufinity-voorpositiviteit
```

After install, restart each client to load the new MCP server.

Remove with `taufinity mcp uninstall --client <name> --label <name>` (also supports `--client all`).

#### Config locations per client

| Client | Path | Top-level key |
|---|---|---|
| `claude-desktop` | macOS `~/Library/Application Support/Claude/claude_desktop_config.json`<br>Windows `%APPDATA%\Claude\claude_desktop_config.json` | `mcpServers` |
| `claude-code` | `~/.claude.json` (all OSes) | `mcpServers` |
| `cursor` | `~/.cursor/mcp.json` (all OSes) | `mcpServers` |
| `vscode` | macOS `~/Library/Application Support/Code/User/mcp.json`<br>Linux `~/.config/Code/User/mcp.json`<br>Windows `%APPDATA%\Code\User\mcp.json` | `servers` |
| `antigravity` | macOS `~/Library/Application Support/Antigravity/mcp.json`<br>Linux `~/.config/Antigravity/mcp.json`<br>Windows `%APPDATA%\Antigravity\mcp.json` | `mcpServers` |

Override any path with the matching env var: `TAUFINITY_DESKTOP_CONFIG`, `TAUFINITY_CLAUDE_CODE_CONFIG`, `TAUFINITY_CURSOR_CONFIG`, `TAUFINITY_VSCODE_CONFIG`, `TAUFINITY_ANTIGRAVITY_CONFIG`.

For Claude Code's project-level `.mcp.json` with a bearer token (rather than the global stdio bridge), use `taufinity mcp login` instead.

Run any command with `--help` for full flag documentation.

### MCP stdio bridge

For automatic token refresh and multi-org setups, use `taufinity mcp stdio`. It forwards JSON-RPC from Claude Desktop over stdio to Studio's `/mcp` endpoint, reloading credentials from disk on every request.

Example Claude Desktop config:

```jsonc
{
  "mcpServers": {
    "taufinity-studio": {
      "command": "taufinity",
      "args": ["mcp", "stdio"]
    }
  }
}
```

**Pinning a specific organization** — if your global CLI config points to a different org than the MCP server should use, pass `--org` with the organization ID:

```jsonc
{
  "mcpServers": {
    "taufinity-acme": {
      "command": "taufinity",
      "args": ["--org", "3", "mcp", "stdio"]
    }
  }
}
```

This sends `X-Organization-ID: 3` on every request, overriding the org embedded in the JWT.

Flags:

- `--org ID` — pin to a specific organization (overrides JWT org).
- `--api-url URL` — override the upstream (defaults to `$TAUFINITY_API_URL`, then config, then `https://studio.taufinity.io`).
- `--timeout DURATION` — per-request timeout (default 5m, accommodates BigQuery-backed tools).

## Banner templates bekijken

```bash
cd mijn-banner-sjabloon
taufinity preview --init   # schrijft voorbeeld.csv met alle velden
taufinity preview          # opent de banner in de browser, ververst bij elke wijziging
```

Het invullen gebeurt op de server met dezelfde code als een echte render, dus je ziet
precies wat een render krijgt, minus de capture. `--row 3` toont een regel uit de CSV
(kopregel = regel 1). Zie `taufinity preview --help`. Voor sjablonen van artikelen van
een site is er `taufinity template preview`.

**Echte render.** De knop "Echte render (normale prijs)" op de pagina vraagt Studio om een
echte render van de regel die je ziet, tegen de normale prijs: PNG 1 credit, GIF van
10 frames 2 credits. De pagina vraagt dat één keer in de browser en onthoudt je
bevestiging (`localStorage`, sleutel `taufinity.preview.renderConfirmed`). Het resultaat
verschijnt onder de banner.

**Bewaken.** Na een wijziging wacht het commando 250 ms tot het stil is en vult dan
opnieuw. Wat niet meegaat in de zip naar Studio (`.gitignore`, `.taufinityignore`,
`.dockerignore`, de vaste uitsluitingen zoals `.env`) start ook geen nieuwe vulling, net
als tijdelijke bestanden van editors (`.swp`, `~`, `.DS_Store`) en `.git`/`node_modules`.
Sla je op terwijl een vulling nog onderweg is, dan vervalt die en telt alleen de nieuwste.

**Prijs.** 1 credit per ontwikkelaar, per sjabloon, per uur bekijken. De sessie loopt 60
minuten vanaf de eerste keer invullen; opslaan daarna in dat uur kost niets. Na elke
keer invullen toont de terminal de stand ("1 credit voor deze template, sessie tot
17:46" of "binnen de sessie tot 17:46, geen extra credit"), en de pagina toont dezelfde
tekst. De tijd is lokale tijd.

**`.taufinity/preview.json`.** Bij het eerste gebruik maakt het commando in de
sjabloonmap `.taufinity/preview.json` met een willekeurige `template_key`. Bewaar dit
bestand bij het sjabloon, dan blijft de sleutel gelijk en telt een nieuwe sessie niet
dubbel. Er staat geen geheim in. De map `.taufinity/` gaat nooit mee in de zip naar
Studio. De sleutel hoort bij de map: het bestand onthoudt welke map, als code (geen pad
of gebruikersnaam). Een gekopieerde of verplaatste map, of een nieuwe clone op een
andere plek, krijgt daarom een eigen sleutel en daarmee een eigen sessie van 1 credit.

**Opslag.** Studio bewaart het sjabloon zelf nooit: het leeft alleen in het geheugen
voor de duur van één verzoek. Bewaard blijft alleen de regel voor de afrekening
(organisatie, ontwikkelaar, sleutel, credits, tijd).

## Code review before you push

`taufinity review` checks your local changes with Taufinity Studio before you push to a branch that deploys. It reads the local git repository only, so it works with GitHub, Bitbucket, GitLab or no git host at all, and it talks to Taufinity Studio and nothing else.

Your organisation must have code review switched on. If it is not, the command says so and tells you to ask your Taufinity contact.

```bash
taufinity review                      # your changes against the base branch
taufinity review src/ docs/index.md   # only these files or folders
taufinity review --staged             # only what is staged
taufinity review --base origin/develop
taufinity review --whole src/api      # the current content of these files, changed or not
```

**Diff mode (default).** The base is the upstream of the current branch, else `origin/main`, else `main`; override it with `--base <ref>`. The working tree is compared with the merge-base of that ref and HEAD, so committed and uncommitted changes are both included, plus new files that are not ignored. `--staged` compares the index instead. `--head <rev>` reviews only the commits up to that revision, leaving uncommitted and untracked files out. Paths are git pathspecs relative to the current directory. When nothing changed (under the paths), the command says so and exits 0 without calling Studio.

**Whole mode.** `--whole` reviews the current content of the files under the paths you give (at least one is required). A review over 1.5 MiB after exclusions is refused locally: narrow the paths.

### Exit codes

`--fail-on critical|high|medium|none` (default `critical`) decides when the command fails.

| Code | Meaning |
| ---- | ------- |
| 0 | No finding at or above `--fail-on`, or nothing to review |
| 1 | A finding at or above `--fail-on`, or a `request_changes` verdict that reports no critical or high finding (the counts are incomplete, so the gate fails rather than passes; not with `--fail-on none`) |
| 2 | An error or a refusal (not switched on, budget reached, not signed in, no result) |

The review text goes to stdout. Progress and a one-line summary (counts per severity, verdict, "partial" when the review stopped early) go to stderr. `--json` prints only the final result line as JSON on stdout. `--repo <name>` overrides the repository name, which defaults to `owner/name` from the origin remote, else the folder name. `--dry-run` shows what would be sent (mode, base, repo, file count, sizes, skipped files, masked count) and sends nothing.

### Pre-push hook

```bash
taufinity review install-hook         # writes .git/hooks/pre-push
taufinity review install-hook --force # replace an existing hook
```

The hook reviews what is being pushed, not your working tree. For each pushed ref, git gives the hook the local commit and the remote's current commit, and the hook runs `taufinity review --fail-on critical --head <local sha> --base <remote sha>`: the commits in that range, without uncommitted or untracked files. A new branch (or a remote commit you have not fetched) is compared with the default base: the upstream, `origin/main` or `main`. Deleting a remote branch reviews nothing. The hook works in git worktrees. The push stops when the review finds a critical issue. If the review cannot run (offline, not signed in, code review not switched on), the hook says so and lets the push go. Skip it once with `git push --no-verify`.

### Staging

```bash
taufinity review --env staging
```

`--env staging`, `--api-url` and `--org` work as for every other command. Reviews are sent to `<Studio URL>/api/code-review/reviews` and nowhere else. `<Studio URL>` is the API URL you configured (default, `--env` or `--api-url`), so only point it at a Studio you trust. Code is only sent over https (plain http only to localhost).

### CI

Set `TAUFINITY_API_KEY` and the command uses it instead of a stored login:

```bash
TAUFINITY_API_KEY=... taufinity review --fail-on high
```

CI checkouts are often shallow: fetch enough history for the merge-base (for example `fetch-depth: 0`) or pass `--base <ref>`.

The key is sent as `X-API-Key` and is never printed, not even with `--debug`. Debug output shows the size of the request, never its content.

### What is never sent

These files are never read and never sent, wherever they are in the repository:

- Env and credential files: `.env`, `*.env`, `.env.*`, `.env-*`, `.envrc`, `.netrc`, `_netrc`, `.pgpass`, `.npmrc`, `.pypirc`, `.git-credentials`, `.dockercfg`, `.docker/config.json`, `credentials.json`.
- Keys and certificates: SSH private keys `id_rsa*`, `id_dsa*`, `id_ecdsa*`, `id_ed25519*` (with their `.pub`), `*.ppk`, and `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`.
- Terraform state and variables: `*.tfstate`, `*.tfstate.backup`, `*.tfvars`, `*.tfvars.json`.
- Local databases: `*.sqlite`, `*.db`, `*.dump`, `*.sql.gz`.
- Generated and vendored files: `node_modules`, `vendor`, `dist` and `build` at the repository root, `*.min.js`, `*.min.css`, `*.map`, and the lock files `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`, `Cargo.lock`, `composer.lock`.

Also never sent:

- Google service-account key files: any `*.json` with `"type": "service_account"` and a `private_key`. They have no fixed name, so these are recognised by their content, which is read locally and not sent.
- Binary files and files over 1 MiB.
- Files ignored by git.

Each skipped file is listed on stderr with the reason. Secrets in what is sent are replaced by `[MASKED]` (cloud and API keys, tokens, JWTs, private key blocks, URL credentials, bearer tokens, and values of 8 or more characters, quoted or not, assigned to names such as `password`, `secret` or `token`); the number masked is printed, never the values. The CLI writes no code or review to disk.

### Cost

Studio keeps the cost of reviews against your organisation. The CLI never prints an amount: after each review it shows how much of the organisation's budget is used, for example `Code review budget used: 6% today, 2% this month`. Amounts are for system admins in Studio.

### From an MCP client

`taufinity mcp stdio` adds a tool, `review_local_changes` (arguments `paths`, `whole`, `base`, `staged`), that runs the same review on the repository in its working directory and returns the review, the counts and the budget line.

## Configuration

Config is resolved in this order (highest priority first):

| Source | Example |
|--------|---------|
| Flag | `--site mysite_com` |
| Environment variable | `TAUFINITY_SITE=mysite_com` |
| Project file | `taufinity.yaml` in project root |
| User config | `~/.config/taufinity/config.yaml` |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `TAUFINITY_SITE` | Default site ID |
| `TAUFINITY_API_URL` | API base URL (default: `https://studio.taufinity.io`) |
| `TAUFINITY_NO_UPDATE_CHECK` | Set to `1` to suppress the background staleness check and warning |
| `TAUFINITY_ORG` | Default organization ID |
| `TAUFINITY_DEBUG` | Set to `1` to log all HTTP requests |
| `TAUFINITY_QUIET` | Set to `1` to suppress output |
| `TAUFINITY_DRY_RUN` | Set to `1` to print API calls without executing |
| `TAUFINITY_ENV` | `staging` targets `https://staging-studio.taufinity.io` (same as `--env staging`) |
| `CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET` | Cloudflare Access service token override, sent to the staging host only. Optional there: the CLI caches the token after reading it once with gcloud (see [Staging](#staging)) |

### Project File (`taufinity.yaml`)

```yaml
site: mysite_com
template: templates/article.html
preview_data: fixtures/article.json
ignore:
  - node_modules/
  - dist/
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
