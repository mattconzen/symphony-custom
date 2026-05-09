---
labels: [orchestrator, dashboard, deviation-fix]
---

# Wire `Orchestrator.RequestRefresh()` to make `POST /api/v1/refresh` real

## Why

Today `POST /api/v1/refresh` returns 202 with a success envelope but is a documented no-op:
the Go orchestrator polls only on its own tick interval. SPEC §14.2 calls this out as a v0
deviation explicitly intended to be lifted. Wiring a real refresh trigger turns the endpoint
honest.

This change focuses on the orchestrator-side plumbing only. A separate change adds a UI
button.

## What changes

1. Add `func (o *Orchestrator) RequestRefresh() bool` to
   `go/internal/orchestrator/orchestrator.go`. Returns true if a fresh refresh was scheduled,
   false if one is already pending (coalesced).

2. Implementation: a buffered (capacity 1) `chan struct{}` field `refreshC` on
   `Orchestrator`. `RequestRefresh` does a non-blocking send (`select { case o.refreshC <-
   struct{}{}: return true; default: return false }`).

3. The existing poll loop's tick `select` block gains a third case: when `refreshC` fires,
   the poll runs immediately (the regular ticker is reset to keep the cadence stable).

4. Update `go/internal/web/api.go`'s `handleAPIRefresh`: call `orch.RequestRefresh()` and set
   `coalesced = !ok` in the response envelope. The 202 status stays.

5. Remove the v0-deviation language from SPEC §14.2 about refresh being a no-op. Replace it
   with the real semantics: "POST /api/v1/refresh schedules an immediate poll. The 202
   envelope's `coalesced` field is true when a refresh was already pending."

## Acceptance

- New test `TestOrchestrator_RequestRefresh` in `internal/orchestrator/orchestrator_test.go`:
  - Returns true on first call (refreshC was empty).
  - Returns false on second call before the poll loop drains it (coalesced).
  - After the poll loop drains, third call returns true again.
- Updated test in `internal/web/api_test.go` covering coalesced=true and coalesced=false
  responses (use a stub orchestrator-like type).
- E2E test `go/e2e/web_test.go` already exercises `POST /api/v1/refresh` — confirm it still
  passes; extend if needed.
- `cd go && go test ./... -count=1 && go vet ./...` clean.

## Spec update

Update SPEC §14.2 to remove the no-op deviation note and document the real coalesced behavior.

## Completion signal

`touch openspec-change/.symphony-done` from the workspace root when done.
