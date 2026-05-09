---
labels:
  - go
  - observability
  - parity
  - future-work
---

# Add observability web dashboard to the Go Symphony binary

## Why

The Elixir reference implementation (`elixir/lib/symphony_elixir_web/`) ships a Phoenix LiveView
dashboard at `GET /` plus a JSON observability API under `/api/v1/`. Operators run
`symphony --port 4000 WORKFLOW.md` and open a browser to see running sessions, retry pressure,
token usage, and live agent activity.

The Go reimplementation in `go/cmd/symphony/` has none of that. Its only observability surface is
the structured JSON log on stdout. There's no HTTP server, no `/api/v1/state`, no live UI. This is
a real feature-parity gap that surfaces the moment anyone tries to run Symphony interactively
against a real tracker — they have nowhere to look.

This proposal adds an embedded HTTP server to the Go binary that mirrors the Elixir dashboard's
surface area, with one stack swap (Phoenix LiveView → htmx + WebSocket) chosen for minimal
deps and to stay close to Symphony's "no infra" ethos.

## What changes

### Routes (parity with Elixir)

| Method | Path                          | Handler                                    | Behavior                                                        |
| ------ | ----------------------------- | ------------------------------------------ | --------------------------------------------------------------- |
| GET    | `/`                           | `dashboardHandler`                         | Server-rendered HTML page (htmx + small CSS, no build step).    |
| GET    | `/ws`                         | `wsHandler`                                | WebSocket endpoint pushing snapshot updates as HTML fragments.  |
| GET    | `/api/v1/state`               | `apiStateHandler`                          | Whole-orchestrator JSON snapshot.                               |
| GET    | `/api/v1/{issue_identifier}`  | `apiIssueHandler`                          | Per-issue JSON detail.                                          |
| POST   | `/api/v1/refresh`             | `apiRefreshHandler`                        | Trigger a tracker refresh; returns `202 Accepted`.              |
| GET    | `/static/{file}`              | `staticHandler`                            | Embedded CSS, htmx.min.js, htmx-ws extension.                   |

JSON shape mirrors Elixir's `SymphonyElixirWeb.Presenter` exactly so consumers can switch runtimes
without changing scrapers. Match field names case-by-case (`counts.running`, `codex_totals.total_tokens`,
`running[].issue_identifier`, etc.).

### Dashboard UI (htmx + WebSocket)

- One HTML template, server-rendered on `GET /`.
- The page connects to `/ws` via the htmx `ws` extension (`hx-ext="ws" ws-connect="/ws"`).
- The orchestrator pushes HTML fragments over the WebSocket whenever an observability event
  fires (matches the Elixir `ObservabilityPubSub.broadcast_update()` model). Each fragment
  targets a named `id="…"` region of the dashboard, replaced via htmx out-of-band swaps.
- Static assets (CSS + htmx) embedded in the binary via `embed.FS`. No external CDN, no build
  step, no asset pipeline.

### Data plumbing

A new `internal/observability/snapshot.go` package — the Go equivalent of the Elixir `Presenter`
module — exposes:

```go
type Snapshot struct {
    Counts        Counts        `json:"counts"`
    CodexTotals   TokenTotals   `json:"codex_totals"`
    RateLimits    any           `json:"rate_limits"` // shape comes from agent runtime
    Running       []RunningEntry `json:"running"`
    Retrying      []RetryEntry  `json:"retrying"`
    GeneratedAt   time.Time     `json:"generated_at"`
}

func BuildSnapshot(orch *orchestrator.Orchestrator) Snapshot { ... }
```

`orchestrator.Orchestrator` grows a `Subscribe() <-chan struct{}` method (or accepts an
`OnUpdate func()` callback) so the web layer can react to dispatch state transitions without
polling. Internal structure mirrors the Elixir `StatusDashboard` GenServer.

### Wiring

- New `internal/web/` package containing the `http.Handler`, templates, static assets, and the
  WebSocket loop.
- New CLI flags on `cmd/symphony/main.go`:
  - `-port <int>`: bind port (default `0` = web disabled, matching the current behavior so the
    addition is opt-in).
  - `-listen <host>`: bind interface (default `127.0.0.1`).
- When `-port > 0`, `main.go` starts an `http.Server` in a goroutine alongside the orchestrator
  Run loop, and shuts it down on the same SIGINT/SIGTERM handler.
- Logs the bound URL on startup: `web dashboard available at http://127.0.0.1:4000/`.

### Spec additions

Add a new `## 14. Observability Web Dashboard` section to `go/SPEC.md` covering:

- §14.1 — When the dashboard is enabled (port > 0), the HTTP server lifecycle.
- §14.2 — Route table (the parity table above), with normative `MUST`s on JSON shape so the API
  remains compatible with the Elixir version.
- §14.3 — The snapshot data contract (field names, types, semantics for `counts`, `codex_totals`,
  `rate_limits`, `running`, `retrying`).
- §14.4 — Real-time update protocol: WebSocket framing, what events trigger a push, htmx
  out-of-band swap conventions.
- §14.5 — Error responses: `{"error": {"code": "...", "message": "..."}}` matching Elixir's
  `SymphonyElixirWeb.ObservabilityApiController.error_response/4`.

## Non-goals

- **No write surface.** No buttons to cancel dispatches, retry runs, or modify config from the UI.
  Symphony stays fundamentally CLI-driven; the dashboard is read-only observability.
- **No auth.** The dashboard binds to `127.0.0.1` by default. Operators who expose it more widely
  are responsible for putting it behind a reverse proxy. (Matches Elixir behavior.)
- **No persistence.** State lives in the orchestrator process; the dashboard reflects what's
  in memory. No database, no log replay.
- **No cross-runtime polish parity.** The Elixir LiveView has carefully-tuned CSS and a "Live /
  Offline" indicator. The Go version aims for functional parity, not pixel parity. Cosmetic
  refinement is a follow-up.
- **No SSE fallback.** WebSocket is the chosen real-time mechanism per design decision; we do not
  ship a polling or SSE alternative.

## Acceptance

- `cd go && go test ./internal/web/... -count=1` passes.
- `cd go && go build ./cmd/symphony` builds with the new flags.
- Running `symphony -port 4000 -workflow WORKFLOW.md` against the same OpenSpec fixture used in
  the bug-#3 e2e run:
  - `curl http://127.0.0.1:4000/api/v1/state` returns a JSON document whose shape matches the
    Elixir `Presenter.state_payload/2` output (verified via golden-file comparison if both
    implementations are run side-by-side against an identical workflow).
  - Browsing to `http://127.0.0.1:4000/` shows a metric grid, rate-limits panel, and running-sessions
    table that update without page reload as the orchestrator dispatches and reconciles.
  - Sending SIGINT shuts down both the orchestrator and the HTTP server cleanly.
- A `go test ./e2e/...` test exercises the dashboard end-to-end against an in-process
  orchestrator with the memory tracker + mock runtime, asserting that:
  1. `GET /api/v1/state` returns a valid snapshot.
  2. A WebSocket client receives at least one update fragment after a seeded issue is dispatched.
  3. `POST /api/v1/refresh` returns 202.
- New §14 in `go/SPEC.md` is reviewed for compatibility with the Elixir API contract.

## Out of scope (file as separate work if desired)

- Per-issue detail page in the UI (Elixir's `JSON details` link goes to the API; a real HTML
  detail view is a v2).
- TLS termination in the binary itself (operators put it behind nginx/Caddy).
- Authentication / authorization.
- Prometheus `/metrics` endpoint (different concern; better as its own change).
- Multi-tenant dashboards (one orchestrator → one binary → one dashboard, by design).

## Stack rationale

| Choice                  | Why                                                                                    |
| ----------------------- | -------------------------------------------------------------------------------------- |
| `net/http` (stdlib)     | Zero deps; sufficient for the route surface.                                           |
| `html/template` (stdlib)| Server-rendered HTML, no asset pipeline, no Go module bloat.                           |
| htmx (vendored)         | One `<script>` tag enables `hx-ext="ws"` + out-of-band swaps. No build step.           |
| WebSocket               | User preference; matches Phoenix LiveView's two-way channel even though the dashboard  |
|                         | is read-only. Library: `github.com/coder/websocket` (lightweight, modern, no v0 deps). |
| `embed.FS`              | Ship CSS + htmx + the WS extension in the binary; preserves single-file deployability. |

## Compatibility note

The Elixir API contract (`SymphonyElixirWeb.Presenter`) is the source of truth for snapshot
shape. The Go implementation MUST emit byte-identical JSON for the same orchestrator state when
both are pointed at the same WORKFLOW.md, or the parity claim is hollow. A golden-file test
comparing snapshots from both runtimes is the recommended verification mechanism, gated behind
`SYMPHONY_RUN_PARITY_TEST=1` since it requires both stacks installed.
