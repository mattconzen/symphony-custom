# Symphony Go

Go reimplementation of the Symphony orchestrator per `go/SPEC.md`.

**Phase 1 baseline — Memory tracker + Mock runtime only.**

## Make targets

| Target   | Description                          |
|----------|--------------------------------------|
| `test`   | `go test ./... -race -count=1`       |
| `vet`    | `go vet ./...`                       |
| `lint`   | `golangci-lint run`                  |
| `run`    | `go run ./cmd/symphony`              |
| `tidy`   | `go mod tidy`                        |

## Quick start

```sh
cd go
go run ./cmd/symphony -workflow WORKFLOW.md
```

Set `SYMPHONY_SEED_ISSUES` to a JSON array of `domain.Issue` objects to pre-seed the
in-memory tracker for smoke testing.

## Web dashboard

The binary ships an opt-in observability dashboard. Pass `-port` to enable it:

```sh
go run ./cmd/symphony -port 4000 -workflow WORKFLOW.md
```

Then open `http://127.0.0.1:4000/` to see the Kanban view (Backlog / Ready / In Progress
/ In Review / Done), live running sessions, retry pressure, token totals, and rate-limit
data — all updating in real time over a WebSocket. The same process exposes a JSON API
under `/api/v1/` for scrapers; see `go/SPEC.md` §14 for the contract.

The dashboard surfaces several write endpoints when the active tracker supports them:

- `POST /api/v1/issues` — create a new work item from the dashboard's "+ New work item"
  button. Supported by memory, markdown, and openspec trackers; Linear/JIRA return 405.
- `GET/PUT /api/v1/issues/{id}/spec` — read / write the OpenSpec proposal for an issue.
  Backed by `<openspec>/changes/<slug>/proposal.md` for the openspec tracker, a sibling
  `<slug>.spec.md` for markdown, and an in-process map for memory.
- `POST /api/v1/issues/{id}/spec/generate` — invoke the configured agent runtime to
  draft a proposal. Returns 202 + `{job_id}`; poll `…/spec/generate/{job_id}` until done.

Flags:

- `-port` (int, default `0` = disabled). Setting `>0` starts the HTTP server alongside
  the orchestrator.
- `-listen` (string, default `127.0.0.1`). Bind interface. Stays loopback by default;
  put a reverse proxy in front if you need to expose it elsewhere.

### GitHub PR reconciler

Adding a `github` block to `WORKFLOW.md` enables the PR reconciler, which periodically
queries GitHub for the merge state of every PR an agent has surfaced via its events. The
Kanban view's "In Review" and "Done" columns reflect this state.

```yaml
github:
  owner: my-org
  repo: my-repo
  token_env: GITHUB_TOKEN          # default
  pr_poll_interval_ms: 60000       # default
```

### Manual repro for the WS status indicator

The dashboard's connection pill goes through `Connecting…` → `Live` → `Reconnecting in
Ns…` → `Disconnected` (after 5 failed reconnect attempts). To exercise:

1. Start `symphony -port 4000 WORKFLOW.md`. Open the dashboard. The pill should briefly
   show "Connecting…" then settle on "Live".
2. SIGINT the symphony process. The pill flips to "Reconnecting in 1s…", counts down,
   and re-tries with exponential backoff. After 5 failures it shows "Disconnected".
3. Restart the binary; the next reconnect attempt succeeds and the pill returns to
   "Live".

## Architecture

See `go/SPEC.md` for the normative spec. Packages mirror the spec sections:

- `internal/domain/` — §4: core types (Issue, Workspace, RunAttempt, LiveSession, RetryEntry)
- `internal/workflow/` — §5: WORKFLOW.md loader
- `internal/config/` — §6: typed config + dynamic reload
- `internal/workspace/` — §9: workspace lifecycle + hooks
- `internal/prompt/` — §12: Liquid template renderer
- `internal/tracker/` — §11: Tracker interface; `memory/` adapter (Phase 1)
- `internal/agent/` — §10: Runtime interface; `mock/` adapter (Phase 1)
- `internal/orchestrator/` — §§7–8: poll loop, dispatch, retry, reconciliation
- `internal/observability/` — §13: structured JSON logger + Snapshot schema
- `internal/web/` — §14: opt-in dashboard, JSON API, and WebSocket update stream
- `cmd/symphony/` — CLI entry point

Later phases add Linear, Markdown, OpenSpec, JIRA trackers and Codex/Claude runtimes.
