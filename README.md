# Taufinity CLI

Command-line tool for the [Taufinity](https://studio.taufinity.io) content platform. Manage templates, trigger playbooks, and configure Claude Code MCP integration.

## Installation

### Public (once repo is public)

```bash
go install github.com/taufinity/cli/cmd/taufinity@latest
```

Requires Go 1.21+. The binary is installed as `taufinity` in your `$GOPATH/bin`.

### Private (current — internal use only)

```bash
export GOPRIVATE=github.com/taufinity/cli
git clone git@github.com:taufinity/cli.git
cd cli
make install
```

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
| `version` | Print version info |

Run any command with `--help` for full flag documentation.

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
