# Tasks

- [x] Extract the shared `<head>` markup from `dashboard.html.tmpl` into `templates/_head.html.tmpl` as a `{{ define "head" }}` block. Update `dashboard.html.tmpl` to `{{ template "head" . }}`.
- [x] Create `templates/issue.html.tmpl` rendering a single issue (running or retrying), with a back-link to `/`.
- [x] In `internal/web/api.go`, factor the per-issue payload builder out of `handleAPIIssue` into a reusable function (e.g. `buildIssuePayload(snap, identifier) (issuePayload, bool)` returning `false` on not found).
- [x] Add `GET /issue/{issue_identifier}` to `internal/web/handlers.go`. Reuse the factored builder. On found → render `issue.html.tmpl` with 200. On not found → render a small 404 template (or reuse `issue.html.tmpl` with an empty payload + flag).
- [x] Update the `dashboard.html.tmpl` table row links: `<a href="/issue/{{ .IssueIdentifier }}">details</a>` (replacing the JSON link).
- [x] Add tests in `internal/web/handler_test.go`: `TestIssueDetailPage_Found`, `TestIssueDetailPage_NotFound`.
- [x] Update SPEC §14.2 to add the new route.
- [x] Run tests + vet + build, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
