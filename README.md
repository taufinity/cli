# Taufinity CLI

Command-line tool for the [Taufinity](https://studio.taufinity.io) content platform. Manage templates, trigger playbooks, and configure Claude Code / Claude Desktop MCP integration.

## Installation

### macOS / Linux (Go 1.21+)

```bash
go install github.com/taufinity/cli/cmd/taufinity@latest
```

The binary is installed as `taufinity` in `$GOPATH/bin` (usually `~/go/bin`). Make sure that directory is in your `$PATH`:

```bash
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

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

#### Security note

`taufinity update` installs from the `main` branch via `go install ...@latest`. Anyone with commit access to `main` ships to all CLI users on their next update. Acceptable for the small internal team today; once we cut tagged releases, the default will move to a tagged version.

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

Opens a browser window for device authorization. Credentials are stored at `~/.config/taufinity/credentials.json`.

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
