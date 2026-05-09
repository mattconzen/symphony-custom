# Tasks

## Design

- [ ] Lock the JSON snapshot schema. Sample the Elixir `Presenter.state_payload/2` output
      against a non-trivial fixture (e.g. memory tracker + 3 seeded issues) and capture it as
      `go/internal/observability/testdata/snapshot.golden.json`.
- [ ] Decide WebSocket library: confirm `github.com/coder/websocket` vs alternatives (avoid
      `gorilla/websocket` archival concerns).
- [ ] Decide CSS approach: vendor the Elixir dashboard's CSS verbatim (one-file copy) or rewrite
      lean. Recommendation: vendor verbatim for v0; refine later.
- [ ] Pick htmx version + pin in `embed.FS` source comment.

## Snapshot plumbing

- [ ] New `internal/observability/snapshot.go`: `Snapshot` struct, `BuildSnapshot(orch)`
      function, JSON-tagged fields matching the Elixir Presenter contract exactly.
- [ ] Extend `internal/orchestrator/orchestrator.go` with a notification mechanism. Two options:
  - Add `(o *Orchestrator) Subscribe() <-chan struct{}` returning a debounced update channel.
  - Add an `OnUpdate func()` callback set via `WithUpdateCallback`.
      Prefer the callback (simpler, no goroutine ownership questions).
- [ ] Fire the notification at the same points the Elixir `StatusDashboard.broadcast_update()`
      fires: dispatch start, turn complete, dispatch finish, reconcile, retry scheduled.
- [ ] Unit-test `BuildSnapshot` against a constructed orchestrator state; assert against the
      golden JSON.

## HTTP server

- [ ] New `internal/web/` package skeleton: `Handler(orch *Orchestrator) http.Handler`.
- [ ] Implement `GET /api/v1/state` → marshal snapshot.
- [ ] Implement `GET /api/v1/{issue_identifier}` → per-issue snapshot or 404
      with `{"error": {"code":"issue_not_found", ...}}`.
- [ ] Implement `POST /api/v1/refresh` → trigger an out-of-band poll; return 202.
- [ ] Implement `GET /` → render `dashboard.html.tmpl` with initial snapshot.
- [ ] Implement `GET /ws` → upgrade to WebSocket, subscribe to orchestrator updates,
      stream HTML fragments on each update; close on client disconnect or context cancel.
- [ ] Implement `GET /static/*` → serve embed.FS assets with correct content-type and
      `Cache-Control: public, max-age=3600`.
- [ ] Implement method-not-allowed and not-found JSON error responses for `/api/v1/*` parity.

## CLI + lifecycle

- [ ] Add `-port` and `-listen` flags to `cmd/symphony/main.go`.
- [ ] When `-port > 0`, construct `internal/web` handler, start `http.Server` in a goroutine,
      log the bound URL.
- [ ] Wire SIGINT/SIGTERM to `server.Shutdown(ctx)` with a small timeout; ensure orchestrator
      and HTTP server both drain before main returns.
- [ ] Default port = `0` (disabled). Confirm existing tests still pass with web off.

## Templates + assets

- [ ] `internal/web/templates/dashboard.html.tmpl` — vendor the Elixir dashboard markup;
      replace EEx with `html/template` syntax.
- [ ] `internal/web/static/dashboard.css` — vendor the Elixir CSS (served from
      `/dashboard.css` in Elixir; reroute to `/static/dashboard.css` here).
- [ ] `internal/web/static/htmx.min.js` + `htmx-ws-ext.min.js` — vendored, pinned versions.
- [ ] `embed.FS` directive in `internal/web/embed.go`.

## Tests

- [ ] Unit tests for each handler using `httptest.NewServer`.
- [ ] WebSocket integration test: spin up the handler with an in-memory orchestrator, dispatch
      a fake issue, assert the WS client receives a fragment within 1s.
- [ ] `go/e2e/web_test.go`: full end-to-end test that boots the orchestrator with memory
      tracker + mock runtime, hits all routes, validates JSON shapes against the golden file.
- [ ] Optional parity test (`SYMPHONY_RUN_PARITY_TEST=1`): boot both Elixir and Go orchestrators
      against the same WORKFLOW.md, hit `/api/v1/state` on both, diff the JSON.

## Spec

- [ ] Draft `go/SPEC.md` §14 (Observability Web Dashboard) with the route table, snapshot
      contract, real-time update protocol, error format.
- [ ] Update §6.4 cheat sheet with the new `port` and `listen` CLI flags.
- [ ] Update `go/README.md` with a "Web dashboard" section showing `symphony -port 4000`.
- [ ] Update `go/WORKFLOW.md` example WORKFLOW.md doesn't change (no new YAML config) — confirm.

## Cleanup

- [ ] Document the parity contract in `internal/observability/snapshot.go` package comment with
      a link to the Elixir Presenter source.
- [ ] Add a one-liner in the top-level `README.md` noting the Go binary now has a dashboard.
