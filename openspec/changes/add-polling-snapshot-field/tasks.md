# Tasks

- [ ] Add `Polling` struct + `Snapshot.Polling` field in `go/internal/observability/snapshot.go`.
- [ ] Add `pollChecking bool` and `lastPollAt time.Time` to `Orchestrator` (`go/internal/orchestrator/orchestrator.go`), guarded by the existing mutex.
- [ ] Toggle `pollChecking` around the tracker poll in the orchestrator's tick loop. Update `lastPollAt` after each poll.
- [ ] Populate `Polling` in `BuildSnapshot` (`go/internal/orchestrator/snapshot.go`).
- [ ] Add `formatNextPoll(ms int) string` helper to `go/internal/web/funcs.go`. Format: `"Next poll in 1.2s"` when ms > 0, `"Checking…"` when checking. Take a struct or two args.
- [ ] Update `go/internal/web/templates/dashboard.html.tmpl` `header-status` sub-template to render the polling state.
- [ ] Update `go/internal/observability/testdata/snapshot.golden.json` with the new field. Use a fixed value like `polling: {checking: false, poll_interval_ms: 2000, next_poll_in_ms: 1500}`.
- [ ] Add `TestBuildSnapshot_Polling` to `go/internal/orchestrator/snapshot_test.go`.
- [ ] Append to `go/SPEC.md` §14.3 documenting the new field.
- [ ] Run `cd go && go test ./... -count=1 && go vet ./...` and confirm clean.
- [ ] Run `touch openspec-change/.symphony-done` from the workspace root.
