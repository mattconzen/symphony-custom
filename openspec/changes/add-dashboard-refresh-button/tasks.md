# Tasks

- [x] Add the `<button class="refresh-button" hx-post="/api/v1/refresh" hx-swap="none">⟳ Refresh</button>` to `dashboard.html.tmpl`. Place it inside or adjacent to the existing `id="header-status"` container so the WebSocket OOB swaps don't accidentally remove it (header-status fragment should NOT contain the button — wrap it outside).
- [x] Add CSS for `.refresh-button` (and `.refresh-button[disabled]`, `.refresh-button:hover`) to `static/dashboard.css`. Match the dashboard's existing visual weight.
- [x] Add a test in `internal/web/handler_test.go` asserting `GET /` body contains the button HTML (specifically `hx-post="/api/v1/refresh"`).
- [x] Manual smoke test: build + run with `-port`, click the button, confirm the orchestrator log shows an immediate poll. (Sibling change `wire-orchestrator-request-refresh` must land first for this to be observable.)
- [x] Run tests + vet, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
