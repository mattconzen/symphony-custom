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

Then open `http://127.0.0.1:4000/` to see live running sessions, retry pressure, token
totals, and rate-limit data — all updating in real time over a WebSocket. The same
process exposes a JSON API under `/api/v1/` for scrapers; see `go/SPEC.md` §14 for the
contract.

Flags:

- `-port` (int, default `0` = disabled). Setting `>0` starts the HTTP server alongside
  the orchestrator.
- `-listen` (string, default `127.0.0.1`). Bind interface. Stays loopback by default;
  put a reverse proxy in front if you need to expose it elsewhere.

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
