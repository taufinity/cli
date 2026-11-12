# Handoff

## Handoff

**To:** claude
**From:** claude session 8a38d47f-b26c-47b4-b309-6281cc944bfb, stopped making progress; this is its one fresh-session retry
**State:** round 4. Verified: T1, T2, T3, T4, T5, T6, T7, T8, T9, T10, T11, T12, T13, T14, T15, T16, T17. Open: 1.
**Next step:** continue HARNESS_TODO.md from the first open item.
**Do not touch:** checklist item text, branches, remotes. Do not push.

### Open items

- T18 Gemini second opinion (last item). Write the branch diff without tests to $SCRATCH_DIR/review.diff (`git diff origin/main...HEAD -- . ':!*_test.go' ':!docs'`), then run /Users/robin/Documents/code/cto-as-a-service/scripts/gemini-review-call.sh --kind code --depth standard --purpose "Review of a CLI command that sends a local git diff to a review API: secret masking, exclusions, streaming NDJSON, exit codes, no cost leak to members" --file $SCRATCH_DIR/review.diff --context-file docs/reviews/2026-11-12-review-command-cto-review.md --output docs/reviews/2026-11-12-review-command-gemini-review.json. The script needs GEMINI_API_KEY from your environment. Do not look for the key anywhere else (no shell config, dotfiles, vaults, Secret Manager). Add "## Gemini second opinion" to the CTO review document (before the Source/Why block). If the script stops because GEMINI_API_KEY is not set, write exactly the line `GEMINI: NOT RUN, GEMINI_API_KEY missing from the harness environment` under that heading, delete any partial JSON output, and tick the item: the manager runs it later. On any other failure record the exact error (never a key or token) and leave the item unticked. When it runs, list the verdict and every finding with a disposition; fix every finding you accept, with a test.

### Last instruction to the previous engine

Harness feedback after round 4. The work is NOT done yet. Continue in this session.

Reopened: these items were ticked but did not pass verification. Fix them, then tick them again.
- T18: Gemini second opinion (last item). Write the branch diff without tests to $SCRATCH_DIR/review.diff (`git diff origin/main...HEAD -- . ':!*_test.go' ':!docs'`), then run /Users/robin/Documents/code/cto-as-a-service/scripts/gemini-review-call.sh --kind code --depth standard --purpose "Review of a CLI command that sends a local git diff to a review API: secret masking, exclusions, streaming NDJSON, exit codes, no cost leak to members" --file $SCRATCH_DIR/review.diff --context-file docs/reviews/2026-11-12-review-command-cto-review.md --output docs/reviews/2026-11-12-review-command-gemini-review.json. The script needs GEMINI_API_KEY from your environment. Do not look for the key anywhere else (no shell config, dotfiles, vaults, Secret Manager). Add "## Gemini second opinion" to the CTO review document (before the Source/Why block). If the script stops because GEMINI_API_KEY is not set, write exactly the line `GEMINI: NOT RUN, GEMINI_API_KEY missing from the harness environment` under that heading, delete any partial JSON output, and tick the item: the manager runs it later. On any other failure record the exact error (never a key or token) and leave the item unticked. When it runs, list the verdict and every finding with a disposition; fix every finding you accept, with a test.
  Why: verifier: docs/reviews/2026-11-12-review-command-gemini-review.json does not exist despite GEMINI_API_KEY being available. The CTO review contains neither a real Gemini verdict nor every finding with a disposition. Next: Generate the test-excluded branch diff, run the specified gemini-review-call.sh command with the existing environment key, save its JSON output, replace the incorrect missing-key line with the verdict and every finding plus disposition, and add tests for every accepted finding that requires a fix.

Still open:
- T18: Gemini second opinion (last item). Write the branch diff without tests to $SCRATCH_DIR/review.diff (`git diff origin/main...HEAD -- . ':!*_test.go' ':!docs'`), then run /Users/robin/Documents/code/cto-as-a-service/scripts/gemini-review-call.sh --kind code --depth standard --purpose "Review of a CLI command that sends a local git diff to a review API: secret masking, exclusions, streaming NDJSON, exit codes, no cost leak to members" --file $SCRATCH_DIR/review.diff --context-file docs/reviews/2026-11-12-review-command-cto-review.md --output docs/reviews/2026-11-12-review-command-gemini-review.json. The script needs GEMINI_API_KEY from your environment. Do not look for the key anywhere else (no shell config, dotfiles, vaults, Secret Manager). Add "## Gemini second opinion" to the CTO review document (before the Source/Why block). If the script stops because GEMINI_API_KEY is not set, write exactly the line `GEMINI: NOT RUN, GEMINI_API_KEY missing from the harness environment` under that heading, delete any partial JSON output, and tick the item: the manager runs it later. On any other failure record the exact error (never a key or token) and leave the item unticked. When it runs, list the verdict and every finding with a disposition; fix every finding you accept, with a test.

Work through every open item until all are ticked in HARNESS_TODO.md.
