---
labels:
  - go
  - dashboard
  - kanban
  - work-management
  - tracker
  - openspec
issues:
  - "#2"
  - "#3"
  - "#4"
  - "#5"
  - "#6"
---

# Go dashboard work-management bundle (issues #2 – #6)

## Why

Five GitHub issues against `mattconzen/symphony-custom` describe a coherent
upgrade to the Go dashboard's work-management surface:

- **#2** No way to add a work item from the UI.
- **#3** No Kanban view; operators can't see lifecycle state at a glance.
- **#4** The "Live / Offline" status pill is unreliable.
- **#5** Visible labels still say "Codex"; the runtime is now provider-agnostic.
- **#6** No way to view/edit the OpenSpec spec attached to a work item, and no
  way to mint one for tasks that don't yet have one.

Implementing them piecemeal would churn the same files (`internal/web/*`,
`internal/tracker/tracker.go`, `internal/domain/`, `cmd/symphony/main.go`,
`go/SPEC.md`) several times and force five overlapping change branches. This
proposal bundles them into one coordinated change.

**Implementation language:** Go only. The Elixir reference implementation
under `elixir/` is **not** modified by this change. Where the existing Go
dashboard pinned itself to Elixir-API parity (see
`openspec/changes/go-web-dashboard/proposal.md`), this change explicitly
breaks parity on the renamed fields and documents the divergence in
`go/SPEC.md`.

## What changes

### 1. Tracker: `CreateIssue` and `HasSpec` (covers #2, #3)

Extend the `tracker.Tracker` interface in
`go/internal/tracker/tracker.go`:

```go
type Tracker interface {
    // ... existing methods ...

    // CreateIssue persists a brand-new issue. Implementations that cannot
    // create issues (read-only sources) MUST return ErrCreateUnsupported.
    CreateIssue(ctx context.Context, draft IssueDraft) (domain.Issue, error)

    // HasSpec reports whether the tracker has an OpenSpec-style spec
    // associated with the issue identifier. Used by the Kanban view to
    // partition Backlog vs Ready. Implementations without a spec concept
    // MUST return false, nil.
    HasSpec(ctx context.Context, id string) (bool, error)
}

type IssueDraft struct {
    Title       string
    Description string
    Labels      []string
}

var ErrCreateUnsupported = errors.New("tracker: create not supported")
```

Per-tracker behavior:

| Tracker  | `CreateIssue`                                                       | `HasSpec`                                              |
| -------- | ------------------------------------------------------------------- | ------------------------------------------------------ |
| memory   | Append a new `domain.Issue` with auto-generated identifier (`MEM-N`). | Always `false`.                                        |
| markdown | Write a new `<root>/<slug>.md` with YAML front-matter + body.       | True iff the file contains an `openspec:` front-matter key OR a sibling `<slug>.spec.md` exists. |
| openspec | `mkdir <root>/changes/<slug>/` and write `proposal.md` + `tasks.md`. | True iff `<root>/changes/<slug>/proposal.md` exists.   |
| linear   | Return `ErrCreateUnsupported`.                                      | Always `false`.                                        |
| jira     | Return `ErrCreateUnsupported`.                                      | Always `false`.                                        |

The slug-from-title rule used by `markdown`/`openspec` is the existing
`domain.Issue.WorkspaceKey()` algorithm applied to the supplied title.

### 2. Domain: PR tracking on `domain.Issue` (covers #3)

Add to `go/internal/domain/issue.go`:

```go
type Issue struct {
    // ... existing fields ...

    PR *PullRequest `json:"pr,omitempty"`
}

type PullRequest struct {
    URL      string     `json:"url"`
    Number   int        `json:"number"`
    State    string     `json:"state"`     // "open" | "merged" | "closed"
    MergedAt *time.Time `json:"merged_at,omitempty"`
    Source   string     `json:"source"`    // "agent_event" | "github_poll"
    UpdatedAt time.Time `json:"updated_at"`
}
```

PR data has two writers:

1. **Agent-event ingest.** Add a new `domain.AgentEvent` kind `pr_link` (URL +
   optional state). The codex and claude runtimes already emit structured
   events; add one helper that normalizes `https://github.com/<owner>/<repo>/pull/<n>`
   strings out of agent stdout/messages and emits the new event. The
   orchestrator records the event onto the running entry and persists it via
   the tracker (`Tracker.SetPullRequest(ctx, issueID, pr)` — new optional
   method behind a type assertion; trackers that don't implement it just
   keep the in-memory copy on the snapshot).

2. **GitHub poll reconciler.** A new `internal/github/` package provides a
   minimal client (stdlib `net/http`, no SDK) with a single method
   `GetPullRequest(ctx, owner, repo, number) (PullRequest, error)`. The
   orchestrator runs a periodic reconciler (interval default 60s, configurable)
   that re-fetches the merge state for every issue with an open PR and
   updates the snapshot. This is the source of truth for "merged".

Configuration (added to WORKFLOW.md front-matter):

```yaml
github:
  owner: mattconzen
  repo: symphony-custom
  token_env: GITHUB_TOKEN          # default
  pr_poll_interval_ms: 60000       # default
```

The reconciler is a no-op when `github` is unset.

### 3. Kanban view section (covers #3)

Add a Kanban grid as a new section above the existing metric grid on
`GET /` (no separate page, no replacement of the ops dashboard). Five
columns, derived per-issue:

| Column      | Predicate                                                                   |
| ----------- | --------------------------------------------------------------------------- |
| Backlog     | not in active runtime, not terminal, `HasSpec == false`                     |
| Ready       | not in active runtime, not terminal, `HasSpec == true`                      |
| In Progress | issue identifier appears in `snapshot.Running` (agents are working)         |
| In Review   | `Issue.PR != nil && PR.State == "open"`                                     |
| Done        | `PR.State == "merged"` OR tracker state is in `tracker.terminal_states`     |

Column cards show the issue identifier, title (truncated), and (where
relevant) PR number / agent runtime / time-in-state. Cards link to the
existing `/issue/{identifier}` page. Done cards link directly to the merged
PR URL.

Implementation lives in:

- `internal/observability/snapshot.go` — extend `Snapshot` with a
  `Kanban []KanbanColumn` field and a `BuildKanban(...)` helper that takes
  the full tracker issue list (a new `Snapshot` input) and partitions.
- `internal/orchestrator/snapshot.go` — call `BuildKanban` after the
  existing snapshot work; the orchestrator already knows running/retrying;
  it now also needs the tracker's "everything else" list, so we add a
  `Tracker.FetchAllIssues(ctx)` helper (delegates to existing methods).
- `internal/web/templates/dashboard.html.tmpl` — new `{{ define "kanban" }}`
  block, included between the hero and metric-grid sections; new
  `id="kanban"` region for WS out-of-band swaps.
- `internal/web/ws.go` — append `"kanban"` to `fragmentTemplates`.

### 4. "New work item" button (covers #2)

UI: a "+ New work item" button anchored next to the existing "⟳ Refresh"
button. Clicking it opens a small modal with two inputs:

- **Title** (single-line, required, max 200 chars).
- **Description** (textarea, optional, plain markdown).

Submission posts to a new endpoint:

```
POST /api/v1/issues
Content-Type: application/json
{ "title": "...", "description": "...", "labels": ["dashboard"] }

201 Created → { "issue_identifier": "MEM-7", "issue_id": "..." }
405 → ErrCreateUnsupported envelope when the active tracker is read-only
```

After 201 the modal closes and the dashboard requests a refresh
(`POST /api/v1/refresh`). If the active tracker returns
`ErrCreateUnsupported`, the button is hidden at render time (the
`dashboardView` exposes a `CanCreateIssue bool`).

### 5. Status indicator robustness pass (covers #4)

Three coordinated fixes for the WS pill in
`internal/web/templates/dashboard.html.tmpl` and the embedded JS:

1. **Server-rendered initial state is `connecting`, not `live`.** Today the
   page lies for the first ~50ms; replace `class="ws-status ws-status-live"
   data-state="live"` with the connecting variant. The JS upgrades to
   `live` on `htmx:wsOpen`.
2. **Add explicit reconnect with exponential backoff.** htmx's WS extension
   does have built-in reconnect, but on `htmx:wsClose` we currently set the
   pill to "Reconnecting…" and never verify it actually reconnects. Replace
   the JS handler with a state machine that:
   - On `wsClose`: schedule reconnect with backoff (1s → 2s → 4s → 8s → 16s,
     cap 30s), display "Reconnecting in {n}s…".
   - On `wsOpen`: reset backoff and switch to "Live".
   - On 5 consecutive failed reconnects: display "Disconnected" (not
     "Reconnecting…") so the operator knows the page is stale.
3. **Drop the dead Phoenix CSS.** Remove `.status-badge-live`,
   `.status-badge-offline`, and the `[data-phx-main].phx-connected` rules
   from `internal/web/static/dashboard.css` — they're leftover from the
   Elixir port and never emit.

### 6. Codex → Agents rename (covers #5)

Replace "Codex" with "Agent" / "Agents" across the Go binary's user-facing
text, JSON contract, and Go field names:

| Surface              | Before                       | After                         |
| -------------------- | ---------------------------- | ----------------------------- |
| Go struct field      | `observability.Snapshot.CodexTotals` | `observability.Snapshot.AgentTotals` |
| JSON key             | `codex_totals`               | `agent_totals`                |
| JSON key             | `codex_session_logs`         | `agent_session_logs`          |
| Template literals    | "Codex update"               | "Agent update"                |
| Template literals    | "Total Codex runtime"        | "Total agent runtime"         |
| Template literals    | "Codex token usage"          | "Agent token usage"           |
| Prometheus metric    | `symphony_codex_tokens_total` | `symphony_agent_tokens_total` |
| Prometheus metric    | `symphony_codex_seconds_running` | `symphony_agent_seconds_running` |

Internal Codex-specific config (`config.Codex`, the codex runtime package,
codex-specific spec sections in `go/SPEC.md` §§5.3.6 / 10) is **not**
renamed — it refers to the actual Codex app-server protocol and that name
is correct.

`go/SPEC.md` §14 is amended with a "**Divergence from Elixir**" callout
noting that the Go API uses `agent_*` keys and is no longer wire-compatible
with `SymphonyElixirWeb.Presenter`. The Elixir implementation is left
untouched per the task constraint.

### 7. View/Edit OpenSpec button (covers #6)

On the existing `/issue/{identifier}` detail page, add a new section
"OpenSpec" with two states:

- **When `HasSpec == true`:** Show the spec content rendered as Markdown,
  with an "Edit" button that swaps the view for an in-place
  CodeMirror-based editor (Markdown mode + side-by-side preview). Save
  posts to `PUT /api/v1/issues/{id}/spec`. The OpenSpec adapter writes the
  body back to `<root>/changes/<slug>/proposal.md`. Markdown adapter writes
  to a sibling `<slug>.spec.md`. Memory tracker stores it in-memory.
- **When `HasSpec == false`:** Show a "Generate spec" button. Clicking it
  posts to `POST /api/v1/issues/{id}/spec/generate`. The handler invokes the
  configured agent runtime (the same `agent.Runtime` the orchestrator
  already loaded — codex or claude) with a hard-coded "draft an OpenSpec
  proposal.md for this issue" prompt and the issue's title + description as
  context. Output is held as a draft (not yet saved); the user reviews in
  the editor, edits if desired, and clicks "Save" to persist via the same
  `PUT` endpoint. Generation runs synchronously with a 5-minute timeout;
  the modal shows a streaming progress indicator driven by the runtime's
  event channel.

CodeMirror 6 is vendored as a precompiled bundle into
`internal/web/static/codemirror.min.js` (~180 KB) and lazy-loaded only on
the issue detail page (not on `/`). License is MIT — added to `NOTICE`.

Endpoints:

```
GET  /api/v1/issues/{id}/spec         → 200 + {"body": "..."} or 404
PUT  /api/v1/issues/{id}/spec         → 204 on save; 409 on stale etag
POST /api/v1/issues/{id}/spec/generate → 202 + {"job_id": "..."} ; events
                                          stream over the existing /ws as
                                          oob fragments scoped to job_id
```

## Acceptance

Per-issue:

- **#2** `POST /api/v1/issues` against a memory tracker returns 201 and the
  new issue appears on the next refresh; against a Linear/JIRA tracker
  returns 405 with the `unsupported_create` envelope; the dashboard hides
  the button in that case.
- **#3** With seeded fixtures producing one issue per Kanban column, `GET /`
  renders five columns containing exactly the seeded issues; the In Review
  column populates after a `pr_link` agent event; the Done column flips
  when the GitHub poller reports merged.
- **#4** Loading `/` shows "Connecting…" for the first frame, then "Live";
  killing the WS server (e.g., restart) drives it through "Reconnecting in
  Ns…" with backoff and finally "Disconnected" after 5 failures; restarting
  the server brings it back to "Live" automatically.
- **#5** `curl /api/v1/state | jq .agent_totals.total_tokens` works;
  `.codex_totals` is absent. `curl /metrics` exposes
  `symphony_agent_tokens_total`. No template references "Codex" except in
  config-explanation copy.
- **#6** OpenSpec tracker: editing `proposal.md` from the browser updates
  the file on disk. Memory tracker: clicking "Generate spec" against an
  agent runtime configured as `mock` returns the mock's canned output; the
  user clicks Save and `HasSpec` flips to true.

Cross-cutting:

- `cd go && go test ./... -race -count=1` is clean.
- `cd go && go vet ./... && golangci-lint run` is clean.
- `cd go && go build ./cmd/symphony` succeeds.
- A new e2e test (`go/e2e/dashboard_kanban_test.go`) exercises the Kanban
  swap end-to-end: seed → poll → assert columns → emit pr_link →
  assert In Review → flip merged → assert Done.

## Spec updates (`go/SPEC.md`)

- §11.1 — Tracker interface gains `CreateIssue` and `HasSpec`. Add
  `IssueDraft` and `ErrCreateUnsupported`.
- §11.x (new subsection per tracker) — describe the `CreateIssue` and
  `HasSpec` semantics for each adapter.
- §4 — `domain.Issue` gains `PR *PullRequest`; `PullRequest` shape defined.
- §13 / §14 — `Snapshot` gains `Kanban []KanbanColumn` and `AgentTotals`
  (the renamed `CodexTotals`). The route table adds `POST /api/v1/issues`,
  `GET /api/v1/issues/{id}/spec`, `PUT /api/v1/issues/{id}/spec`,
  `POST /api/v1/issues/{id}/spec/generate`.
- §14 — add a **Divergence from Elixir** callout under the route table:
  "The Go dashboard's JSON API uses `agent_*` field names where the Elixir
  port used `codex_*`. Consumers that need to support both runtimes must
  translate."
- §16 (new) — GitHub PR reconciler: configuration block, polling cadence,
  rate-limit handling.
- §17 (new) — Spec generation: how the configured agent runtime is invoked,
  prompt template, timeout, draft-then-save semantics.

## Out of scope

- Authentication / authorization on the new write endpoints. The dashboard
  still binds to 127.0.0.1 by default; operators exposing it more broadly
  must put it behind a proxy that handles auth.
- Any change to the Elixir reference implementation. Even though renaming
  `codex_totals` → `agent_totals` breaks parity, we leave Elixir alone per
  the task constraint and document the divergence.
- A real-time editor for the dashboard's metric panels.
- Multi-user editor collisions beyond simple etag-based 409 responses.
- Replacing the existing ops dashboard with the Kanban — Kanban is a new
  section on the same page.

## Completion signal

`touch openspec/changes/go-dashboard-work-management/.symphony-done` when
all per-issue acceptance criteria pass and the spec updates land.
