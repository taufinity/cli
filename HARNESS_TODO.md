# Checklist: cli-banner-preview-sessions

Managed by the agent harness. Tick an item (`- [x]`) only when it is fully done.
Every ticked item is independently verified; items that fail are reopened with the reason.
Do not edit or remove items. Add work you discover under "## Discovered" as `- [ ] text`.

## Items

- [x] **T1**: `taufinity preview --init`: write voorbeeld.csv next to the template (semicolon separated, UTF-8): the header with every field from report.json "fields", and one row with the values of the generated row labelled "voorbeeld" (report.json rows[].values); fields ending in _url or _file are left empty. Refuses to overwrite an existing voorbeeld.csv without --force. Test TestBannerPreviewInit with subtests writes_header_and_row, uses_server_values, file_fields_left_empty, refuses_overwrite.
- [x] **T2**: Template key per folder: on first use create <template folder>/.taufinity/preview.json holding {"template_key": "<random id, 22 chars of [A-Za-z0-9_-]>"} and reuse it on every later run; send it as template_key on every fill. The .taufinity/ folder is never put in the template ZIP. Test TestBannerPreviewTemplateKey with subtests created_once_and_reused, sent_with_every_fill, not_in_template_zip.
- [x] **T3**: Show the charge: in the terminal after each fill one line, "1 credit voor deze template, sessie tot HH:MM" when charge.credits is 1, else "binnen de sessie tot HH:MM, geen extra credit" (only printed when it changes, not on every refresh); on the preview page an element with id "charge" showing the same text. Test TestBannerPreviewCharge with subtests prints_first_charge, quiet_within_session, page_shows_charge.
- [x] **T4**: Help and README: `taufinity preview --help` and the README section explain the price (1 credit per developer per template per hour of previewing, later saves in that hour free), the .taufinity/preview.json file (keep it with the template so the key stays the same; it holds no secret), and that the template itself is never stored by Studio.

## Discovered

