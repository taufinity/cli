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
| `mcp login` | Write credentials to `.mcp.json` for Claude Code |
| `mcp install` | Add Taufinity Studio to Claude Desktop's config |
| `mcp uninstall` | Remove Taufinity Studio from Claude Desktop's config |
| `mcp print` | Print the Claude Desktop server JSON block to stdout |
| `mcp stdio` | Run a stdio MCP bridge to Studio's `/mcp` endpoint (for stdio-only clients) |
| `update [--check\|--rollback]` | Update taufinity to the latest version (or check/rollback) |
| `version` | Print version info |

### Claude Desktop one-command install

After `taufinity auth login`:

    taufinity mcp install

Writes a server entry to Claude Desktop's config (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS, `%APPDATA%\Claude\claude_desktop_config.json` on Windows). Restart Claude Desktop to load it.

**Note:** `mcp install` bakes the current bearer token into the config. When your session expires, Claude Desktop will silently start returning auth errors. Re-run `taufinity mcp install --force` to refresh it, or use the stdio bridge below which refreshes tokens automatically.

For Claude Code (`.mcp.json`), use `taufinity mcp login` instead.

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
