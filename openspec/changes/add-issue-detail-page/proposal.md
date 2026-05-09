---
labels: [dashboard, ux]
---

# Add per-issue HTML detail page

## Why

`/api/v1/{issue_identifier}` returns JSON only. The dashboard's "Running sessions" table has
a "JSON details" link that points to a raw JSON document — a hostile UX. A real HTML page
showing one issue's full snapshot, formatted, addresses the most common operator question:
"what's happening with WEB-1 right now?"

Neither Elixir nor Go currently has this; building it on the Go side moves us past parity.

## What changes

1. Add a new route `GET /issue/{issue_identifier}` in `internal/web/handlers.go` (alongside
   the existing `GET /` dashboard route). It renders a new template `issue.html.tmpl` against
   the per-issue payload (the same `issuePayload` struct `internal/web/api.go` already builds —
   factor it out so both the JSON API and HTML view consume it).

2. Add `templates/issue.html.tmpl` (in `internal/web/templates/`):
   - Reuse the dashboard's `<head>` chrome (CSS, htmx) by extracting it into a `_head.html.tmpl`
     partial that both `dashboard.html.tmpl` and `issue.html.tmpl` `{{ template "head" . }}`
     in. (The current dashboard.html.tmpl is a single file; the partial extraction is part of
     this change.)
   - Body: a header with the identifier, state, and back-link to `/`. Below: a "session"
     section (session_id, started_at, turn_count, last_event, last_message, workspace_path)
     and a "tokens" section (input/output/total). If the issue is in retry, show a "retry"
     section (attempt, due_at, error). If the issue is not found, render a small 404 page
     instead of returning JSON.

3. Update the `running` and `retrying` table rows in `dashboard.html.tmpl` to link to
   `/issue/{{ .IssueIdentifier }}` instead of `/api/v1/{{ .IssueIdentifier }}`.

4. The page does NOT subscribe to the WebSocket; it's a snapshot view. (We can revisit live
   updates for the detail page in a follow-up change if there's demand.) A "Refresh" link
   posts to the same endpoint as the dashboard's refresh button (sibling change) so the page
   can be reloaded after.

## Acceptance

- `GET /issue/{seeded_id}` returns 200 with HTML containing the identifier, state, session id,
  and token totals.
- `GET /issue/missing` returns 404 with HTML (not JSON), still using the dashboard chrome.
- The dashboard's JSON-detail link now points to `/issue/...`, not `/api/v1/...`.
- Both `dashboard.html.tmpl` and `issue.html.tmpl` parse via the same `template.ParseFS` call.
- `cd go && go test ./internal/web/... -count=1` clean.

## Spec update

Add to SPEC §14.2 the new route `GET /issue/{issue_identifier}` (HTML; 200 or 404).

## Completion signal

`touch openspec-change/.symphony-done` when done.
