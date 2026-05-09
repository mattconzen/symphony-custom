---
labels: [observability, dashboard, parity]
---

# Add `polling` field to `observability.Snapshot`

## Why

The Elixir `Presenter` exposes a `polling: {checking?, next_poll_in_ms, poll_interval_ms}` map
on every snapshot, and the Elixir dashboard renders a "next poll in 1.2s" / "checking…" pulse
in the header. The Go snapshot has no equivalent, so the Go dashboard appears frozen between
dispatch events. For a tool whose explicit purpose is observability of a poll loop, this is
the most visible parity gap.

## What changes

1. Add a new struct `Polling` in `go/internal/observability/snapshot.go`:

   ```go
   type Polling struct {
       Checking       bool `json:"checking"`         // true when a poll is in flight
       PollIntervalMs int  `json:"poll_interval_ms"` // configured tick interval
       NextPollInMs   int  `json:"next_poll_in_ms"`  // time until next scheduled poll, ms
   }
   ```

2. Add `Polling Polling \`json:"polling"\`` to `Snapshot`.

3. In `go/internal/orchestrator/snapshot.go`, populate `Polling` from orchestrator state:
   - `Checking` is true when the poll goroutine is mid-fetch. Add a small mutex-guarded
     `pollChecking bool` to `Orchestrator` and toggle it around the tracker fetch in the
     poll loop.
   - `PollIntervalMs` comes from `o.cfg.Polling.IntervalMs`.
   - `NextPollInMs` is `max(0, lastPollAt + interval - now)` in ms. Track `lastPollAt time.Time`
     under the same mutex.

4. Update the dashboard template fragment `header-status` (in
   `go/internal/web/templates/dashboard.html.tmpl`) to render the polling state — show
   "Checking…" when `Checking` is true, otherwise "Next poll in <N>s" using a new
   `formatNextPoll` template func (add to `go/internal/web/funcs.go`).

5. Update `go/internal/observability/testdata/snapshot.golden.json` to include the new field.

## Acceptance

- New unit test in `internal/orchestrator/snapshot_test.go` verifying `Polling` is populated:
  - Default state: `Checking=false`, `PollIntervalMs` matches config, `NextPollInMs` ∈ [0, interval].
  - During a synthetic poll: `Checking=true`.
- `internal/observability/snapshot_test.go` golden-file test updated with the new field.
- Dashboard fragment renders "Next poll in N.Ns" when idle and "Checking…" mid-poll.
- `cd go && go test ./... -count=1 && go vet ./...` clean.

## Spec update

Append to `go/SPEC.md` §14.3 (Snapshot Data Contract): document the `polling` field,
its three subfields, and the semantics (especially that `next_poll_in_ms` is best-effort
and may be 0 just before a poll fires).

## Completion signal

When the acceptance criteria are met, run `touch openspec-change/.symphony-done` from the
workspace root.
