# Tasks

- [ ] Add `refreshC chan struct{}` (buffered cap 1) to `Orchestrator` struct.
- [ ] Initialize `refreshC` in the `New` constructor with `make(chan struct{}, 1)`.
- [ ] Add `func (o *Orchestrator) RequestRefresh() bool` doing a non-blocking send.
- [ ] Extend the poll loop's tick `select` in `Run` (or wherever the ticker case lives) with `case <-o.refreshC:` that triggers an immediate poll.
- [ ] Reset the ticker after a forced refresh so the regular cadence resumes from the refresh moment (avoid double-poll right after).
- [ ] Update `internal/web/api.go` `handleAPIRefresh` to call `orch.RequestRefresh()` and set the `coalesced` field accordingly.
- [ ] If the web handler doesn't currently have a way to call into the orchestrator beyond `Snapshot()`, extend the small interface in `internal/web/handlers.go` (add `RequestRefresh() bool` to `snapshotSource` or a new interface).
- [ ] Add `TestOrchestrator_RequestRefresh`.
- [ ] Update `internal/web/api_test.go` to cover coalesced true/false.
- [ ] Update SPEC §14.2 to remove the no-op deviation note; document real coalesced semantics.
- [ ] Run tests + vet + build, confirm clean.
- [ ] `touch openspec-change/.symphony-done`.
