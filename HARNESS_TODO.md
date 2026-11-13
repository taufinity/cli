# Checklist: cli-preview-render-button

Managed by the agent harness. Tick an item (`- [x]`) only when it is fully done.
Every ticked item is independently verified; items that fail are reopened with the reason.
Do not edit or remove items. Add work you discover under "## Discovered" as `- [ ] text`.

## Items

- [x] **T1**: Local endpoint POST /api/render on the page server (commands/preview_server.go): POST only (405 otherwise), X-Preview-Token required (403 without or wrong, constant-time compare), foreign Origin refused (403), one render at a time (409 with a Dutch message). Test TestPreviewRenderEndpointGuards with subtests wrong_method, missing_token, wrong_token, foreign_origin, one_at_a_time.
- [x] **T2**: The page HTML carries the per-process token (a meta tag or inline value the page script reads); the banner's own origin/frame never receives it. Test TestPreviewRenderTokenOnlyInPage with subtests page_has_token, banner_origin_has_no_token.
- [x] **T3**: Upload step in a new file commands/preview_render.go: tar.gz of the same file set the preview zips (reuse the preview's file selection), POST /api/templates/upload with field archive via api.Client, read archive_id. Test TestPreviewRenderUpload with subtests same_files_as_preview, secrets_excluded, upload_error_shown (a 413 JSON error becomes a Dutch message containing the server text).
- [x] **T4**: Render step in commands/preview_render.go: POST /api/creatives/render with archive_id, width/height from the current bannerResult, format png or gif (gif adds frames 10), variables = current row keyed by CSV header; then GET download_url with the same client and keep the file in the preview's temp dir. Test TestPreviewRenderRequest with subtests png_body, gif_body_has_10_frames, variables_from_current_row, refused_422_shows_server_text, not_achieved_reported.
- [x] **T5**: The rendered file is served to the page from the page server (e.g. GET /render/<n>, local-only, correct Content-Type image/png or image/gif) and the page shows it under the banner with the status, bytes and notes. Test TestPreviewRenderResultServed with subtests png_served, gif_served, unknown_404.
- [x] **T6**: Page UI in commands/preview_page.html: button 'Echte render (normale prijs)', a format choice PNG (default) / GIF, a modal on the first render stating it is a real render at the normal price with the chosen format's price ('1 credit per stilstaand beeld' or '2 credits voor een animatie van 10 frames'), Bevestigen and Annuleren buttons; confirming stores localStorage taufinity.preview.renderConfirmed=1 and starts the render; later clicks skip the modal; while rendering the button is disabled and shows progress text.
- [x] **T7**: Browser test (chromedp, headless Chrome, file commands/preview_render_browser_test.go) TestPreviewRenderBrowser driving the real page served by the real page server against an httptest fake Studio, subtests: first_click_shows_modal, cancel_sends_nothing (fake Studio saw no request, localStorage unset), confirm_renders_and_remembers (one upload + one render seen, result image shown on page, localStorage set), second_click_skips_modal (after reload, no modal, a second render is sent), gif_choice_sends_gif. It must not skip when Chrome is present.
- [x] **T8**: Help text: the PRIJS section of the preview command's Long help (commands/preview.go) gets one paragraph on the button: a real render at the normal price, PNG 1 credit, GIF of 10 frames 2 credits, asked once in the browser and then remembered. Also mention it in docs if the repo documents the preview (grep docs/ for 'preview').

## Discovered

