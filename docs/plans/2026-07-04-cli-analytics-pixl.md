# CLI Analytics Pixel Implementation Plan

**Created:** 2026-07-04
**Status:** In Progress
**Context:** Add fire-and-forget analytics tracking to the Taufinity CLI so we see install/update/error events before customers report them. GCS pixel bucket + access logs = zero backend code, zero auth.

## Architecture

A public GCS bucket (`taufinity-cli-pixl`) serves a 1×1 transparent GIF at event paths. Every request lands in the access-log bucket. Shell scripts `curl` the URL. The Go binary has a thin `internal/pixl` package. No auth required — the URL is the key.

```
/v1/install          /v1/uninstall        /v1/update_check
/v1/up_to_date       /v1/update_available /v1/updated
/v1/update_error     /v1/error            /v1/script_error
```

Query params on every hit: `v` (version), `os`, `arch`, `did` (anonymous UUID from device.json).

**Two repos:**
- `ai-site-gen/terraform/sitegen-cli-pixl/` — GCS buckets, IAM, pixel objects
- `taufinity/cli` (installer worktree) — `internal/pixl/`, updated shell scripts, updated CI ldflags

## Global Constraints

- GCP project: `content-gen-484211`
- Region: `europe-west4` (Netherlands, GDPR)
- TF state bucket: `content-gen-484211-terraform-state`, prefix `sitegen-cli-pixl`
- Pixel URL injected at build time via ldflags (`PixlBaseURL`) — no-op when empty (local builds)
- Shell pixl calls: `curl -sf ... &>/dev/null &` — async, never block, never fail the script
- Go pixl calls: goroutine with 3s timeout — async, fire-and-forget

## Plan

### Phase 1 — Terraform (ai-site-gen repo)

- [ ] **1.1** Create `terraform/sitegen-cli-pixl/variables.tf`
  ```hcl
  variable "project_id" { default = "content-gen-484211" }
  variable "region"     { default = "europe-west4" }
  ```

- [ ] **1.2** Create `terraform/sitegen-cli-pixl/main.tf`
  ```hcl
  terraform {
    required_version = ">= 1.0"
    required_providers { google = { source = "hashicorp/google", version = ">= 5.0" } }
    backend "gcs" {
      bucket = "content-gen-484211-terraform-state"
      prefix = "sitegen-cli-pixl"
    }
  }
  provider "google" { project = var.project_id, region = var.region }

  # Pixel bucket (public read, access-logged)
  resource "google_storage_bucket" "pixl" {
    name          = "taufinity-cli-pixl"
    location      = var.region
    force_destroy = false
    uniform_bucket_level_access = true
    logging { log_bucket = google_storage_bucket.pixl_logs.name }
    versioning { enabled = false }
  }

  # Logs sink
  resource "google_storage_bucket" "pixl_logs" {
    name          = "taufinity-cli-pixl-logs"
    location      = var.region
    force_destroy = false
    uniform_bucket_level_access = true
    lifecycle_rule {
      action { type = "Delete" }
      condition { age = 90 }
    }
  }

  # Public read on pixel bucket
  resource "google_storage_bucket_iam_member" "pixl_public" {
    bucket = google_storage_bucket.pixl.name
    role   = "roles/storage.objectViewer"
    member = "allUsers"
  }

  # Grant GCS logging SA write access to log bucket
  data "google_storage_project_service_account" "gcs_sa" {}
  resource "google_storage_bucket_iam_member" "pixl_logs_writer" {
    bucket = google_storage_bucket.pixl_logs.name
    role   = "roles/storage.legacyBucketWriter"
    member = "serviceAccount:${data.google_storage_project_service_account.gcs_sa.email_address}"
  }

  # 1×1 transparent GIF pixel objects for each event
  locals {
    events = [
      "v1/install", "v1/uninstall",
      "v1/update_check", "v1/up_to_date", "v1/update_available",
      "v1/updated", "v1/update_error",
      "v1/error", "v1/script_error",
    ]
    # Minimal 1×1 transparent GIF (base64 decoded inline)
    pixel_b64 = "R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="
  }

  resource "google_storage_bucket_object" "pixl" {
    for_each     = toset(local.events)
    bucket       = google_storage_bucket.pixl.name
    name         = each.key
    content      = base64decode(local.pixel_b64)
    content_type = "image/gif"
  }
  ```

- [ ] **1.3** Create `terraform/sitegen-cli-pixl/outputs.tf`
  ```hcl
  output "pixl_base_url" {
    value = "https://storage.googleapis.com/${google_storage_bucket.pixl.name}"
  }
  output "logs_bucket" {
    value = google_storage_bucket.pixl_logs.name
  }
  ```

- [ ] **1.4** `terraform init && terraform plan -out=tfplan` — review, confirm no surprises
- [ ] **1.5** `terraform apply tfplan`
- [ ] **1.6** Note the `pixl_base_url` output — goes into CI ldflags

### Phase 2 — Go: `internal/pixl` package (taufinity-cli-installer worktree)

- [ ] **2.1** Create `internal/pixl/pixl.go`
  ```go
  package pixl

  import (
      "encoding/json"
      "fmt"
      "net/http"
      "os"
      "path/filepath"
      "runtime"
      "time"
  )

  // PixlBaseURL is injected at build time via -ldflags.
  // Empty in local builds → all calls are no-ops.
  var PixlBaseURL string

  // Version is shared with commands package via ldflags.
  var Version string

  func Enabled() bool { return PixlBaseURL != "" }

  // Fire sends a GET to {PixlBaseURL}/{event}?params in a goroutine.
  // Never blocks. Never surfaces errors (analytics must not affect UX).
  func Fire(event string, extra map[string]string) {
      if !Enabled() {
          return
      }
      go func() {
          params := baseParams()
          for k, v := range extra {
              params[k] = v
          }
          url := fmt.Sprintf("%s/%s?%s", PixlBaseURL, event, encodeParams(params))
          client := &http.Client{Timeout: 3 * time.Second}
          req, err := http.NewRequest(http.MethodGet, url, nil)
          if err != nil {
              return
          }
          req.Header.Set("User-Agent", fmt.Sprintf("taufinity-cli/%s", Version))
          resp, err := client.Do(req)
          if err != nil {
              return
          }
          resp.Body.Close()
      }()
  }

  func baseParams() map[string]string {
      return map[string]string{
          "v":    Version,
          "os":   runtime.GOOS,
          "arch": runtime.GOARCH,
          "did":  deviceID(),
      }
  }

  func deviceID() string {
      home, err := os.UserHomeDir()
      if err != nil {
          return "unknown"
      }
      path := filepath.Join(home, ".config", "taufinity", "device.json")
      data, err := os.ReadFile(path)
      if err != nil {
          return "unknown"
      }
      var d struct {
          DeviceID string `json:"device_id"`
      }
      if err := json.Unmarshal(data, &d); err != nil || d.DeviceID == "" {
          return "unknown"
      }
      return d.DeviceID
  }

  func encodeParams(params map[string]string) string {
      var parts []string
      for k, v := range params {
          parts = append(parts, k+"="+v)
      }
      // deterministic order not required — this is analytics
      result := ""
      for i, p := range parts {
          if i > 0 { result += "&" }
          result += p
      }
      return result
  }
  ```

- [ ] **2.2** Run `go build ./...` — must compile clean

- [ ] **2.3** Wire into `commands/update.go` — fire `updated` on success, `update_error` on failure:
  ```go
  // after successful update:
  pixl.Fire("v1/updated", map[string]string{"from": currentVersion, "to": newVersion})

  // on error:
  pixl.Fire("v1/update_error", map[string]string{"err": err.Error()})
  ```

- [ ] **2.4** Add ldflags to `.github/workflows/release.yml`:
  ```yaml
  PIXL_BASE_URL: ${{ secrets.PIXL_BASE_URL }}
  ```
  ```
  -X 'github.com/taufinity/cli/internal/pixl.PixlBaseURL=${PIXL_BASE_URL}'
  -X 'github.com/taufinity/cli/internal/pixl.Version=${VERSION}'
  ```

- [ ] **2.5** Add `PIXL_BASE_URL` secret to the `taufinity/cli` GitHub repo Actions secrets
  (value: `https://storage.googleapis.com/taufinity-cli-pixl`)

- [ ] **2.6** Run `go test ./...` — all green

### Phase 3 — Shell scripts

- [ ] **3.1** Update `installer/payload/usr/local/bin/taufinity-update-check`

  Add `fire_pixl()` helper and trap, wire into existing case:
  ```bash
  PIXL_BASE="https://storage.googleapis.com/taufinity-cli-pixl"

  fire_pixl() {
    local event="$1"; shift
    local params="os=darwin&arch=$(uname -m)"
    local v
    v=$("$BINARY" version 2>/dev/null | awk '/^taufinity v/{gsub(/\+.*$/,"",$2); print substr($2,2)}') || v="unknown"
    params="${params}&v=${v}"
    local did_file="$HOME/.config/taufinity/device.json"
    if [ -f "$did_file" ]; then
      local did
      did=$(python3 -c "import json,sys; print(json.load(open('$did_file')).get('device_id','unknown'))" 2>/dev/null || echo "unknown")
      params="${params}&did=${did}"
    fi
    while [ $# -gt 0 ]; do params="${params}&$1"; shift; done
    curl -sf "${PIXL_BASE}/${event}?${params}" &>/dev/null &
  }

  # Trap unexpected script errors
  trap 'fire_pixl "v1/script_error" "exit_code=$?"' ERR
  ```

  Update case block:
  ```bash
  fire_pixl "v1/update_check"
  case "$CHECK_EXIT" in
    0)
      log "Up to date"
      fire_pixl "v1/up_to_date"
      ;;
    1)
      log "Update available — firing notification"
      notify_update
      fire_pixl "v1/update_available"
      ;;
    *)
      log "ERROR (exit $CHECK_EXIT): ${OUTPUT:-update check failed}"
      fire_pixl "v1/error" "exit_code=$CHECK_EXIT"
      "$BINARY" agent report-error \
        --exit-code "$CHECK_EXIT" \
        --message "${OUTPUT:-update check failed}" \
        2>/dev/null || true
      ;;
  esac
  ```

- [ ] **3.2** Update `installer/scripts/postinstall` — fire `install` on success, `install_error` on failure

  ```bash
  PIXL_BASE="https://storage.googleapis.com/taufinity-cli-pixl"

  fire_pixl_root() {
    local event="$1"; shift
    local params="os=darwin&v=unknown"
    while [ $# -gt 0 ]; do params="${params}&$1"; shift; done
    curl -sf "${PIXL_BASE}/${event}?${params}" &>/dev/null & 2>/dev/null || true
  }
  ```

  Wrap the launchctl section:
  ```bash
  if ! launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null; then
    echo "Warning: could not load io.taufinity.cli agent (will load on next login)" >&2
    fire_pixl_root "v1/install_error" "step=bootstrap"
  fi

  fire_pixl_root "v1/install"
  ```

  (postinstall runs as root — no `$HOME`, device.json unavailable, so `v=unknown` is fine for now)

- [ ] **3.3** Update Homebrew tap CI step in `.github/workflows/release.yml` to add `zap` stanza with uninstall pixel to `Casks/taufinity.rb`:
  ```ruby
  zap script: {
    executable: "/bin/bash",
    sudo:        false,
    args:        ["-c", "curl -sf 'https://storage.googleapis.com/taufinity-cli-pixl/v1/uninstall?os=darwin' &>/dev/null || true"],
  }
  ```
  Add this via `sed` in the CI tap-update step after the sha256/version bump.

- [ ] **3.4** Restore executable bits: `git update-index --chmod=+x` on both shell scripts

### Phase 4 — Build, release, verify

- [ ] **4.1** Commit everything in the installer worktree
- [ ] **4.2** Push to `origin/main`
- [ ] **4.3** Tag `v0.6.14` at `origin/main` from the main CLI repo
- [ ] **4.4** Watch CI: both `release` and `macos-pkg` jobs green
- [ ] **4.5** `brew upgrade --cask taufinity` to install v0.6.14
- [ ] **4.6** Run `/usr/local/bin/taufinity-update-check` manually
- [ ] **4.7** Check GCS access logs: `gsutil ls gs://taufinity-cli-pixl-logs/`

## Failure Routing

| Phase | On failure |
|-------|-----------|
| TF plan shows unexpected destroy | STOP — ask Robin |
| `go build` fails | Fix compilation, don't proceed |
| Shell scripts syntax error | `bash -n <script>` to catch, fix first |
| CI macos-pkg fails | Check logs, fix, retag |
| Pixl not appearing in logs | Check bucket ACLs, curl manually |

## Files Modified

- `ai-site-gen/terraform/sitegen-cli-pixl/` — new module (3 files)
- `taufinity-cli/internal/pixl/pixl.go` — new package
- `taufinity-cli/commands/update.go` — fire updated/update_error
- `taufinity-cli/.github/workflows/release.yml` — PIXL_BASE_URL ldflag + zap stanza
- `taufinity-cli/installer/payload/usr/local/bin/taufinity-update-check` — fire_pixl helper
- `taufinity-cli/installer/scripts/postinstall` — install/install_error pixel
