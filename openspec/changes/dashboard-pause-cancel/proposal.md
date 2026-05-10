---
labels:
  - go
  - dashboard
  - orchestration
  - safety
issues:
  - "follow-up to #2-#6 bundle"
---

# Dashboard pause / cancel for in-flight dispatches

## Why

The dashboard is read-only beyond create-issue and edit-spec. There's no
kill-switch: an agent that's burning tokens, looping, or about to do
something the operator regrets has to be killed by SIGINTing the whole
process. That's a footgun for any real-world deployment.

This change adds two minimal turn-level controls to every running issue:

- **Pause** — let the current turn finish, then hold the claim and stop
  dispatching new turns until the operator clicks Resume.
- **Cancel** — cancel the run context immediately (the agent's
  `ctx.Done()` fires), release the claim, do **not** schedule a retry.
  The issue stays at its current tracker state.

Per-tool-call approval and per-risky-op modals are explicitly out of
scope; this proposal targets the smallest useful safety primitive.

## What changes

### State model

`runEntry` (per-issue running record) gains:

```go
type runState int

const (
    runStateRunning runState = iota
    runStatePauseRequested
    runStatePaused
    runStateCancelRequested
)

type runEntry struct {
    // ... existing fields ...
    state    runState
    pauseCh  chan struct{}   // closed when Resume fires
}
```

Lifecycle:

- **Pause**: button → handler → `o.requestPause(issueID)` flips state to
  `runStatePauseRequested`. The turn-loop checks this between turns; on
  hit, it closes `pauseCh` is **NOT** closed (it's created on Pause), the
  loop calls `<-pauseCh`, and the goroutine blocks until Resume re-creates
  / closes it.
- **Resume**: handler closes `pauseCh`. The loop returns from its block,
  flips state back to `runStateRunning`, and continues with the next turn.
- **Cancel**: handler calls `runEntry.cancel()` (the existing
  `context.CancelFunc`) and sets `runStateCancelRequested`. The
  in-flight turn observes `ctx.Done()` and unwinds. `dispatchOne`'s
  retry-on-error path is suppressed when `runStateCancelRequested` is
  set.

Pause does **not** interrupt the in-flight turn — that would corrupt the
agent's session state. The pause takes effect *between* turns.

### Endpoints

```
POST /api/v1/issues/{id}/pause     → 202; idempotent
POST /api/v1/issues/{id}/resume    → 202; 409 if not paused
POST /api/v1/issues/{id}/cancel    → 202; idempotent
```

All return the existing JSON envelope on error. 404 when the identifier
isn't currently running.

### Snapshot

`observability.RunningEntry` gains:

```go
RunState string `json:"run_state"`  // "running" | "pause_requested" | "paused" | "cancel_requested"
```

Surfaced in the JSON API and in the dashboard's running-sessions table
as a status chip.

### Dashboard UI

Each running-sessions table row gains two buttons:

- **Pause** when `run_state == "running"`. Disables and changes label
  to "Pausing…" while `pause_requested`. Becomes **Resume** when
  `paused`.
- **Cancel** always available (with a confirm dialog).

Both buttons POST to the matching endpoint and trigger an immediate
`/api/v1/refresh`. Live state is reflected via the existing WS
fragment swap on the `running-sessions` region.

### Persistence

When run with the sibling `durable-state-json` change enabled, paused
runs survive restart: on reload, any entry with state `paused` keeps its
state; the per-issue goroutine is not respawned (an explicit Resume
re-dispatches the issue). Cancel is final and not persisted (the entry
is removed from the running map at cancel time).

### Tracker-state semantics

By design, neither Pause nor Cancel transitions the tracker state. The
issue stays in its current tracker-side state. Operators who want to
move a paused issue back to "Todo" do that via the tracker UI itself.
This keeps the orchestrator's responsibility narrow and avoids
race-prone state writebacks.

### Spec updates

- `go/SPEC.md` §7 (Orchestration State Machine) gains §7.5 "Pause /
  Cancel" describing the lifecycle.
- §14.2 lists the three new endpoints.
- §14.3 documents the `run_state` field.

## Acceptance

- `POST /api/v1/issues/{id}/pause` against a running issue: dashboard
  shows `pause_requested`, then `paused` after the current turn
  completes; no further turns run.
- `POST /api/v1/issues/{id}/resume` against the same issue: the next
  turn dispatches; dashboard reflects `running` again.
- `POST /api/v1/issues/{id}/cancel` against a running issue: agent
  context cancels within ~1s, the entry leaves the running map, no
  retry is scheduled.
- A paused issue with the sibling durable change enabled survives a
  restart: the dashboard still shows it as `paused` and Resume re-
  dispatches it.
- `cd go && go test ./internal/orchestrator/pause_test.go -count=1`
  passes (covers all three transitions + double-pause idempotency +
  resume-without-pause 409).
- `cd go && go test ./... -race -count=1` clean (verifies pause-channel
  + cancel race-free).

## Out of scope

- Per-tool-call approval prompts.
- Per-risky-op modals (allowlist of dangerous commands).
- Per-issue token / wall-clock budgets.
- Graceful pause **mid**-turn (would require runtime-protocol changes
  the agents don't support).
- Automatic Cancel on budget exceeded (no budget exists).
- Bulk Pause / Cancel ("pause all running issues").

## Completion signal

`touch openspec/changes/dashboard-pause-cancel/.symphony-done`.
