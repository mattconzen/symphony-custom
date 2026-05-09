# Tasks

- [ ] Add the `<button class="refresh-button" hx-post="/api/v1/refresh" hx-swap="none">⟳ Refresh</button>` to `dashboard.html.tmpl`. Place it inside or adjacent to the existing `id="header-status"` container so the WebSocket OOB swaps don't accidentally remove it (header-status fragment should NOT contain the button — wrap it outside).
- [ ] Add CSS for `.refresh-button` (and `.refresh-button[disabled]`, `.refresh-button:hover`) to `static/dashboard.css`. Match the dashboard's existing visual weight.
- [ ] Add a test in `internal/web/handler_test.go` asserting `GET /` body contains the button HTML (specifically `hx-post="/api/v1/refresh"`).
- [ ] Manual smoke test: build + run with `-port`, click the button, confirm the orchestrator log shows an immediate poll. (Sibling change `wire-orchestrator-request-refresh` must land first for this to be observable.)
- [ ] Run tests + vet, confirm clean.
- [ ] `touch openspec-change/.symphony-done`.
