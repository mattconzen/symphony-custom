# Tasks

- [x] Add `refreshC chan struct{}` (buffered cap 1) to `Orchestrator` struct.
- [x] Initialize `refreshC` in the `New` constructor with `make(chan struct{}, 1)`.
- [x] Add `func (o *Orchestrator) RequestRefresh() bool` doing a non-blocking send.
- [x] Extend the poll loop's tick `select` in `Run` (or wherever the ticker case lives) with `case <-o.refreshC:` that triggers an immediate poll.
- [x] Reset the ticker after a forced refresh so the regular cadence resumes from the refresh moment (avoid double-poll right after).
- [x] Update `internal/web/api.go` `handleAPIRefresh` to call `orch.RequestRefresh()` and set the `coalesced` field accordingly.
- [x] If the web handler doesn't currently have a way to call into the orchestrator beyond `Snapshot()`, extend the small interface in `internal/web/handlers.go` (add `RequestRefresh() bool` to `snapshotSource` or a new interface).
- [x] Add `TestOrchestrator_RequestRefresh`.
- [x] Update `internal/web/api_test.go` to cover coalesced true/false.
- [x] Update SPEC §14.2 to remove the no-op deviation note; document real coalesced semantics.
- [x] Run tests + vet + build, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
