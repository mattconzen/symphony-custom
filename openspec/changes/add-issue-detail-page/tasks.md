# Tasks

- [ ] Extract the shared `<head>` markup from `dashboard.html.tmpl` into `templates/_head.html.tmpl` as a `{{ define "head" }}` block. Update `dashboard.html.tmpl` to `{{ template "head" . }}`.
- [ ] Create `templates/issue.html.tmpl` rendering a single issue (running or retrying), with a back-link to `/`.
- [ ] In `internal/web/api.go`, factor the per-issue payload builder out of `handleAPIIssue` into a reusable function (e.g. `buildIssuePayload(snap, identifier) (issuePayload, bool)` returning `false` on not found).
- [ ] Add `GET /issue/{issue_identifier}` to `internal/web/handlers.go`. Reuse the factored builder. On found → render `issue.html.tmpl` with 200. On not found → render a small 404 template (or reuse `issue.html.tmpl` with an empty payload + flag).
- [ ] Update the `dashboard.html.tmpl` table row links: `<a href="/issue/{{ .IssueIdentifier }}">details</a>` (replacing the JSON link).
- [ ] Add tests in `internal/web/handler_test.go`: `TestIssueDetailPage_Found`, `TestIssueDetailPage_NotFound`.
- [ ] Update SPEC §14.2 to add the new route.
- [ ] Run tests + vet + build, confirm clean.
- [ ] `touch openspec-change/.symphony-done`.
