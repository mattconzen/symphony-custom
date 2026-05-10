# Tasks

Group order is intentional: tracker + domain plumbing first, then the
HTTP/template/CSS layer that depends on it, then end-to-end tests.

## 1. Tracker interface + per-adapter implementations (issues #2, #3)

- [ ] Add `IssueDraft`, `ErrCreateUnsupported`, `CreateIssue`, and `HasSpec` to `tracker.Tracker` in `go/internal/tracker/tracker.go`.
- [ ] Implement `CreateIssue` and `HasSpec` on `tracker/memory/memory.go`. `CreateIssue` auto-generates `MEM-N` identifiers under a mutex; `HasSpec` returns false.
- [ ] Implement `CreateIssue` on `tracker/markdown/adapter.go` (write `<root>/<slug>.md` with YAML front-matter); implement `HasSpec` (front-matter `openspec:` flag OR sibling `<slug>.spec.md`).
- [ ] Implement `CreateIssue` on `tracker/openspec/adapter.go` (`mkdir <root>/changes/<slug>/`, write `proposal.md` + `tasks.md`); implement `HasSpec` (file existence check on `proposal.md`).
- [ ] Stub `CreateIssue` on `tracker/linear/` and `tracker/jira/` to return `ErrCreateUnsupported`. Stub `HasSpec` to return false.
- [ ] Add `Tracker.FetchAllIssues(ctx)` helper or equivalent so the orchestrator can list every issue (running + retrying + everything else) for Kanban partitioning. Implement on each adapter.
- [ ] Unit tests per adapter for the new methods.

## 2. Domain + PR plumbing (issue #3)

- [ ] Add `PullRequest` struct and `PR *PullRequest` field on `domain.Issue` in `go/internal/domain/issue.go`.
- [ ] Add `pr_link` agent event kind in `internal/agent/agent.go` (or wherever `AgentEvent` lives); document the schema.
- [ ] Add a normalizer in `internal/agent/internal/` that scans agent stdout/messages for `https://github.com/<owner>/<repo>/pull/<n>` URLs and emits `pr_link` events. Wire into the codex and claude runtimes.
- [ ] Add `internal/github/` package: minimal `Client` with `GetPullRequest(ctx, owner, repo, number) (PullRequest, error)`. Stdlib `net/http`, no SDK. Honor `GITHUB_TOKEN` from env (configurable env name).
- [ ] Add `github` config block to `internal/config/config.go` and the WORKFLOW.md loader in `internal/workflow/`. Defaults: `pr_poll_interval_ms: 60000`, `token_env: GITHUB_TOKEN`.
- [ ] Add a PR reconciler goroutine in `internal/orchestrator/`: every `pr_poll_interval_ms`, fetch PR state for every issue with an open PR; update snapshot.
- [ ] No-op when `github` config is absent.
- [ ] Unit tests: PR-link extraction, GitHub client (httptest), reconciler (faked clock + faked client).

## 3. Snapshot + Kanban derivation (issue #3)

- [ ] Add `KanbanColumn` and `KanbanCard` types to `internal/observability/snapshot.go`.
- [ ] Implement `BuildKanban(allIssues []domain.Issue, snap Snapshot) []KanbanColumn` per the predicate table in the proposal.
- [ ] Wire `BuildKanban` into `internal/orchestrator/snapshot.go` so `Snapshot.Kanban` is populated on every snapshot.
- [ ] Update `internal/observability/testdata/snapshot.golden.json` and `snapshot_test.go`.

## 4. Codex → Agent rename (issue #5)

- [ ] Rename `Snapshot.CodexTotals` → `Snapshot.AgentTotals` in `internal/observability/snapshot.go`. Update JSON tag to `agent_totals`.
- [ ] Rename `issueLogs.CodexSessionLogs` → `AgentSessionLogs` in `internal/web/api.go`. Update JSON tag to `agent_session_logs`.
- [ ] Update every call-site (`internal/orchestrator/`, `internal/web/`, tests).
- [ ] Update template strings in `internal/web/templates/dashboard.html.tmpl` and `issue.html.tmpl`: "Codex update" → "Agent update", "Total Codex runtime" → "Total agent runtime", "Codex token usage" → "Agent token usage".
- [ ] Rename Prometheus metrics in `internal/web/handlers.go`: `symphony_codex_tokens_total` → `symphony_agent_tokens_total`, `symphony_codex_seconds_running` → `symphony_agent_seconds_running`. Update the HELP text.
- [ ] Update fixture files (snapshot golden, api_test.go).
- [ ] Add a "Divergence from Elixir" callout in `go/SPEC.md` §14.

## 5. Status indicator robustness (issue #4)

- [ ] In `internal/web/templates/dashboard.html.tmpl`, change initial pill markup to the connecting variant (`ws-status-connecting`, label "Connecting…", `data-state="connecting"`).
- [ ] Replace the inline `<script>` WS state machine with one that:
    - Schedules reconnect on `htmx:wsClose` with backoff (1s → 2s → 4s → 8s → 16s, cap 30s).
    - Resets backoff on `htmx:wsOpen` and shows "Live".
    - After 5 consecutive failures, switches to "Disconnected" (terminal) and stops scheduling further attempts.
    - Triggers reconnect by removing/re-adding the `ws-connect` attribute (htmx-supported pattern).
- [ ] Remove dead `.status-badge-live`, `.status-badge-offline`, and `[data-phx-main]` rules from `internal/web/static/dashboard.css`.
- [ ] Tests: a JS-free server-side test (`handler_test.go`) asserting the initial markup is the connecting variant. A Playwright/headless test is out of scope; instead, document manual repro steps in `go/README.md`.

## 6. Kanban dashboard section (issue #3)

- [ ] Add `{{ define "kanban" }}` block in `dashboard.html.tmpl` rendering 5 columns from `.Kanban`. Each card links to `/issue/{identifier}`; Done cards link to the merged PR URL.
- [ ] Add `id="kanban"` section in the dashboard body, between the hero and the metric grid.
- [ ] Append `{"kanban", "kanban"}` to `fragmentTemplates` in `internal/web/ws.go`.
- [ ] Add CSS for the kanban grid in `internal/web/static/dashboard.css` (5-column grid, responsive collapse to 1 column under 720px).
- [ ] Handler test asserting the kanban section renders with seeded fixtures; one issue per column.

## 7. New work item button + endpoint (issue #2)

- [ ] Add `POST /api/v1/issues` handler in `internal/web/api.go`. Decodes `{title, description, labels}`, calls `tracker.CreateIssue`, returns 201 with the new issue identifier. Maps `ErrCreateUnsupported` → 405 with the existing error envelope.
- [ ] Add `dashboardView.CanCreateIssue bool` populated by checking the tracker's create capability at render time (sentinel call OR a `Tracker.SupportsCreate() bool` method — pick the lighter path).
- [ ] Add a "+ New work item" button in the dashboard hero, conditional on `CanCreateIssue`.
- [ ] Add a small modal (no JS framework — vanilla HTML `<dialog>` + minimal CSS) with title input + description textarea + Submit button. Submit POSTs JSON via `fetch`, then triggers `/api/v1/refresh` and closes.
- [ ] Handler tests: 201 happy path, 405 unsupported, 400 bad request (empty title), 500 on tracker error.

## 8. View/Edit OpenSpec endpoints + UI (issue #6)

- [ ] Add `GET /api/v1/issues/{id}/spec` reading the spec body via a new `tracker.SpecReader` interface (optional). 200 + `{"body": "...", "etag": "..."}` or 404.
- [ ] Add `PUT /api/v1/issues/{id}/spec` writing via `tracker.SpecWriter`. 204 on success, 409 on stale etag.
- [ ] Add `POST /api/v1/issues/{id}/spec/generate`: invokes the configured `agent.Runtime` with a hard-coded "draft an OpenSpec proposal.md" prompt template (lives in `internal/web/specgen.go`). 5-minute timeout. Returns 202 + `{"job_id": "..."}`. Streams progress as oob fragments scoped to `id="spec-generate-<job_id>"` over the existing `/ws`.
- [ ] Implement `SpecReader`/`SpecWriter` on the openspec adapter (read/write `<root>/changes/<slug>/proposal.md`).
- [ ] Implement on the markdown adapter (sibling `<slug>.spec.md`).
- [ ] Implement on the memory adapter (in-memory map, lost on restart — flagged in the proposal).
- [ ] Linear/JIRA: not implemented in this change; the UI shows "Spec editing not supported for the active tracker."
- [ ] Vendor CodeMirror 6 bundle into `internal/web/static/codemirror.min.js` (~180KB). Add to `NOTICE` (MIT).
- [ ] Add OpenSpec section to `templates/issue.html.tmpl` with two states: render markdown when present + Edit button; "Generate spec" button when absent. Lazy-load CodeMirror only when Edit is clicked.
- [ ] Handler tests for each endpoint; integration test for generate-then-save against the mock runtime.

## 9. Cross-cutting

- [ ] Update `go/SPEC.md` per the "Spec updates" section in the proposal: §11.1 (tracker interface), §11.x (per-adapter), §4 (domain.Issue.PR), §13/§14 (Snapshot.Kanban + AgentTotals + new routes), §14 divergence callout, §16 (PR reconciler), §17 (spec generation).
- [ ] Update `go/README.md` with the new WORKFLOW.md `github:` block and the manual repro steps for the WS reconnect indicator.
- [ ] `cd go && go test ./... -race -count=1`
- [ ] `cd go && go vet ./...`
- [ ] `cd go && golangci-lint run`
- [ ] `cd go && go build ./cmd/symphony`
- [ ] Smoke test: run `symphony -port 4000 WORKFLOW.md`, click "+ New work item", confirm new card appears in Backlog; click "Generate spec" against mock runtime, confirm draft shows up; click Edit, save, confirm the file on disk updates; kill and restart the dashboard and confirm the indicator transitions through Connecting → Reconnecting → Live → Disconnected as appropriate.

## 10. Completion

- [ ] `touch openspec/changes/go-dashboard-work-management/.symphony-done`.
