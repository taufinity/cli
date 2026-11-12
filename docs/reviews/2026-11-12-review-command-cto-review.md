# Review of `taufinity review` (slice 1b)

INTERNAL USAGE, CONFIDENTIAL

Scope: the working tree change set for the new `taufinity review` command (the branch has no
commits of its own yet, so the review covers the uncommitted diff against `origin/main`).
Blast radius: internal CLI that sends source code to Taufinity Studio, so security and cost
scrutiny are high; there is no database, no infrastructure change and no release change.
Checklist applied: the CTO PR review phases (red flags, bug pattern scan, security,
performance, cost protection, test coverage, architecture, documentation).

Red flags checked: large change (yes, about 2,500 lines of which half is tests), security
sensitive (secrets handling, auth header), cost sensitive (a paid request), no migrations, no
configuration or infrastructure changes.

## CTO review

### Secrets or code leaking to logs, debug output or disk

- **P2, request body in debug output.** The existing `authed` helper logs a 200 byte snippet of
  the body, which for a review would be source code. Disposition: **fixed**. `PostStream` has its
  own logging that records the body size only, and the API key and bearer token are never logged.
  Proven by `TestPostStream/debug_redacts_body_and_key` (internal/api).
- **P2, secret values reaching the request.** Disposition: **fixed** by design and proven by
  `TestReviewRequestBody/no_secret_in_body` (a planted token is absent from the captured request,
  the masked count is printed, the value is not) and `TestMask` (one subtest per pattern).
- **P2, excluded files reaching the request, including through the diff of a deleted file.**
  Disposition: **fixed**. Exclusion is by name before any diff is produced, so a deleted `.env`
  does not leak through its removed lines. Proven by `TestExclusions/deleted_secret_not_in_diff`,
  `TestExclusions/tracked_changes_are_filtered_too` and
  `TestReviewRequestBody/excluded_env_not_in_body`.
- **P2, a repository's own git config running a program.** A `core.fsmonitor` setting runs a hook
  on `git diff`. Disposition: **fixed**. Every git call passes `core.fsmonitor=false` and the
  content diff uses `--no-ext-diff --no-textconv`. Covered by the collection tests, which run
  every path through this helper.
- **P3, the settings call with a stored login logs the first 12 characters of the bearer token
  with `--debug`.** This is the behaviour of the shared `GetWithAuth` helper for every command.
  Disposition: **accepted**, out of scope for this slice; the review POST and the API key path do
  not log any part of a credential.
- **P3, the masked count is the sum over the diff and the file contents**, so one secret in a
  changed file is counted twice ("masked 2 secret value(s)"). Disposition: **accepted**: the
  specification defines the payload total that way, and over-counting is the safe direction.
- **P3, unquoted assignments (`password: hunter2hunter2` in YAML) are not masked.** The rule is
  specified as quoted values of 8 or more characters. Disposition: **accepted**; the README now
  says "quoted values", and the exclusion list keeps the usual secret files out entirely.
- **P3, file path names are not masked** (a path that itself contains a token). Disposition:
  **accepted**: the diff headers are masked, the `files` keys are identifiers.
- Disk: nothing is written. The only file the command creates is the hook the user asks for.
  No temp files, no cache. Covered by reading the code paths; no test writes are needed.

### USD reaching a member

- **P1 candidate, `cost_usd` in `--json` output.** Disposition: **fixed**. The terminal line is
  stripped of `cost_usd` before it can be printed. Proven by `TestReadStream/raw_has_no_cost_usd`
  and `TestReviewCostDisplay/json_strips_cost_usd`.
- **P2, other cost shaped fields added later by the service would pass through `--json`.**
  Disposition: **fixed**. Any key containing `cost` or `usd`, at any depth, is removed. Proven by
  `TestReadStream/raw_strips_any_cost_or_usd_key`.
- Human output never reads the cost: the result struct has no cost field, the budget line decodes
  two percentage fields of the settings answer and nothing else, and the MCP tool builds its text
  from the same fields. Proven by `TestReviewCostDisplay/no_usd_anywhere` (normal, `--json`,
  `--fail-on`) and `TestMCPReviewLocalChanges/result_has_no_usd`.
- **P3, the review markdown is service text.** If the service ever writes an amount into the
  markdown, the CLI prints it. Disposition: **accepted**; the server side gap is tracked
  separately and the CLI must not compute or filter prose.

### Any path to a non-Studio host

- The only request targets are `POST /api/code-review/reviews` and `GET /api/code-review/settings`
  on the Studio API URL. Proven by `TestReviewTalksOnlyToStudio/only_studio_paths`,
  `env_staging_targets_staging`, `org_flag_sets_header`, `no_endpoint_flag` and
  `no_direct_call_to_the_review_service` (no source file names the review service path).
- **P2, redirects.** A redirect would carry the credentials and the body to another host.
  Disposition: **fixed**. The streaming client never follows a redirect (`ErrUseLastResponse`);
  a 3xx is reported as a refusal with its status. Proven by
  `TestPostStream/does_not_follow_redirects`.
- **P2, a typo in the API URL could send source code over plain http.** Disposition: **fixed**.
  The command refuses any scheme but https, except http to localhost for a dev server. Proven by
  `TestReviewRefusesCleartext`.
- **P3, `--api-url`, `TAUFINITY_API_URL` and the config `api_url` choose which Studio receives
  the review.** These are the global mechanisms the specification requires the command to honour
  (`--env staging`, `--api-url`). Disposition: **accepted**; no review specific override exists.

### Exit codes

- 0 no finding at or above `--fail-on`, 1 a finding at or above it, 2 any error or refusal;
  `none` never exits 1. Proven by `TestReviewCommand/fail_on_*`, `error_line_exit_2`,
  `stream_without_terminal_exit_2`, `fail_on_invalid_exit_2` and `TestReviewRefusals`.
- `main.go` maps a typed `ExitError` to the process exit code and prints "Error:" only when the
  message was not already printed (exit 1 prints nothing extra). Every other command keeps exit 1.
  A Cloudflare Access block inside an `ExitError` still gets its own wording.
- **P2, the pre-push hook would block every push when the review cannot run** (Studio down, not
  signed in, code review not switched on, which is exit 2). Disposition: **fixed**. The hook
  stops the push only on exit 1 and otherwise prints why and lets the push through. Proven by
  `TestInstallHook/hook_stops_the_push_only_on_findings`. This is a judgment call: a hook that
  fails closed on outages trains people to use `--no-verify` for everything.
- **P3, Ctrl-C exits 2, not 130.** Disposition: **accepted**, specified.

### Large diffs and cost protection

- The POST is not idempotent and costs money: no retries on any status. Proven by
  `TestPostStream/no_retry_on_429`, `no_retry_on_503` and `TestReviewRefusals` (exactly one POST
  per refusal). The only repeat is the single session renewal after a 401, which is refused
  before anything runs (`TestPostStream/renews_once_on_401`, `second_401_is_returned`).
- No client timeout beyond the context; the read is line by line with no 64 KiB token limit
  (`TestReadStream/line_over_4mib`).
- **P2, memory before the limit check.** The 1.5 MiB limit was checked after everything had been
  read, so `--whole .` on a big repository could read gigabytes first. Disposition: **fixed**.
  A running tally aborts as soon as the diff passes 1572864 bytes or the file contents pass the
  rest of Studio's 8 MiB body limit; there is a final 8 MiB check on the encoded body. Proven by
  `TestCollectWhole/too_large_refused`, `TestCollectDiff/diff_over_limit_refused` and
  `TestCollectDiff/files_over_budget_refused` (a tiny diff over many large files).
- Per file: over 1 MiB is skipped, and a large file is never read in full (only its first 8 KiB
  for the binary check). Proven by `TestExclusions/over_1mib_skipped`.
- **P2, `--base` or a tool argument that looks like a git option** (the MCP tool takes `base`
  from a model, which can be steered by repository text). Disposition: **fixed**: a base starting
  with `-` is refused, and paths always follow `--`. Proven by `TestResolveBaseRejectsOptions`.
- **P3, a stalled stream with no keepalive waits until Ctrl-C.** Disposition: **accepted**:
  specified (no timeout other than the context); Studio sends keepalives.
- **P3, diff mode now also refuses a diff over 1.5 MiB locally**, where the specification states
  the rule under `--whole`. Disposition: **accepted**: Studio refuses such a diff upstream anyway
  (`diff_too_large`), and failing locally saves the request.

### Other findings

- **P3, shallow CI checkouts have no merge-base with the base branch.** The command then exits 2
  naming what was tried and suggesting `--base`. Disposition: **fixed in the docs** (README CI
  section: fetch full history or pass `--base`).
- **P3, `install-hook` returns exit 1 on its own errors** (existing hook, not a repository), as
  every other command does. Disposition: **accepted**.
- Architecture: one code path. The command and the MCP tool share `reviewRun.execute`; the
  stdio bridge changed by two small hooks (a call interception before forwarding, and appending
  the tool to the first `tools/list` page) and the recorded tool catalog still holds the
  upstream list only. Every other frame passes through unchanged
  (`TestMCPReviewLocalChanges/other_calls_pass_through`).
- Dependencies: none added.
- Documentation: README section "Code review before you push" covers both modes, paths, exit
  codes, the hook, staging, CI, what is never sent, cost as a percentage and the switch.

## Gemini second opinion

GEMINI: NOT RUN, GEMINI_API_KEY missing from the harness environment

Source: agent manager (153142e6)
Why: code review slice 1b CLI command
