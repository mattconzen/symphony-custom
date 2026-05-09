---
labels: [dashboard, ux]
---

# Add manual refresh button to the dashboard

## Why

The dashboard has no UI to trigger `POST /api/v1/refresh`. Operators who notice stale data
have no recourse but to wait for the next tick. Adding a small button in the dashboard header
that posts to `/api/v1/refresh` (via htmx) closes the gap and exercises the new
`Orchestrator.RequestRefresh` plumbing landed in the sibling change.

This change depends on `wire-orchestrator-request-refresh` being landed first — without it,
the button posts to a no-op endpoint, which is harmless but gives a worse demo.

## What changes

1. In `go/internal/web/templates/dashboard.html.tmpl`, add a `<button>` inside the existing
   `<div id="header-status">` (or adjacent to it):

   ```html
   <button class="refresh-button"
           hx-post="/api/v1/refresh"
           hx-swap="none"
           title="Trigger an immediate poll">
     ⟳ Refresh
   </button>
   ```

2. Add CSS for `.refresh-button` to `go/internal/web/static/dashboard.css`. Match the visual
   weight of the existing status badge. Subtle, not flashy. Disabled state via `[hx-request]`
   selector to dim during the in-flight request.

3. Confirm htmx is loaded (it already is, via `<script src="/static/htmx.min.js">`). No new
   asset needed.

4. The button uses `hx-swap="none"` because the next WebSocket fragment push (after the
   forced poll completes) will refresh the dashboard naturally. No DOM target needed.

## Acceptance

- Manual smoke: open the dashboard, click the button, observe a poll fire in the orchestrator
  log within 100ms (assuming the sibling change is landed). The dashboard's metric grid
  updates via the WebSocket.
- New test in `internal/web/handler_test.go` that does `GET /` and asserts the body contains
  `hx-post="/api/v1/refresh"`.
- `cd go && go test ./internal/web/... -count=1` clean.

## Spec update

None required (§14.2 already specifies the route; this is a UI addition).

## Completion signal

`touch openspec-change/.symphony-done` when done.
