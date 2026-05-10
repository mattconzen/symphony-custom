# Tasks

## 1. State model

- [x] Add `runState` enum + `pauseCh chan struct{}` to `runEntry` in `internal/orchestrator/orchestrator.go`.
- [x] Add helper methods on `*Orchestrator`:
    - `requestPause(issueID string) error`
    - `resume(issueID string) error`
    - `requestCancel(issueID string) error`
    - `runStateOf(issueID string) (string, bool)`
- [x] All four take the orchestrator mutex; mutate state atomically.

## 2. Turn-loop integration

- [x] In `runTurnLoop` (in `internal/orchestrator/dispatch.go`), at the top of each iteration, check pause state. If `pause_requested`, flip to `paused`, then block on `pauseCh`. When the channel is closed (resume), flip back to `running` and continue.
- [x] In the same loop, check `cancel_requested` between turns; on hit, return early (no retry scheduled).
- [x] In `dispatchOne`'s retry-on-error path, skip retry scheduling when run state is `cancel_requested`.

## 3. Endpoints

- [x] Add `POST /api/v1/issues/{id}/pause`, `/resume`, `/cancel` to `internal/web/handlers.go`.
- [x] Map errors:
    - identifier not running → 404 `{"error":{"code":"not_running",...}}`
    - resume on non-paused → 409 `{"error":{"code":"not_paused",...}}`
    - all 2xx returns `{queued:true, identifier, state}` envelope.

## 4. Snapshot

- [x] Add `RunState` field to `observability.RunningEntry`.
- [x] Populate from `runEntry.state` in `runEntryToSnapshot`.

## 5. Dashboard UI

- [x] Update `dashboard.html.tmpl` running-sessions table: add a status-chip column showing `RunState` and an actions column with Pause/Resume/Cancel buttons.
- [x] Inline JS: button clicks POST to the corresponding endpoint then trigger `/api/v1/refresh`.
- [x] Cancel button uses `confirm()` to avoid accidental clicks.
- [x] CSS: paused-row dim styling.

## 6. Durable state integration (optional, when sibling change is enabled)

- [x] When durable store is wired, persist `runState` and `pauseCh`-equivalent metadata in `orchestrator.json`.
- [x] On load, reinstate paused entries (no goroutine respawn — Resume re-dispatches).
- [x] Cancel is not persisted (entry removed from running map).

## 7. Tests

- [x] `internal/orchestrator/pause_test.go`:
    - Pause + Resume round-trip with a fake runtime emitting events.
    - Double-pause is idempotent.
    - Resume on never-paused → error.
    - Cancel cancels the run context (`ctx.Done()` fires).
    - Cancel suppresses the existing retry-on-error path.
- [x] `internal/web/`: handler tests for each endpoint (success, 404, 409).
- [x] Race-detector clean (`go test ./... -race`).

## 8. Docs

- [x] `go/SPEC.md` §7.5 + §14.2 + §14.3.
- [x] `go/README.md` mention of the new buttons.

## 9. Completion

- [x] `touch openspec/changes/dashboard-pause-cancel/.symphony-done`.
