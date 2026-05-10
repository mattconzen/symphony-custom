# Tasks

- [x] Add `Polling` struct + `Snapshot.Polling` field in `go/internal/observability/snapshot.go`.
- [x] Add `pollChecking bool` and `lastPollAt time.Time` to `Orchestrator` (`go/internal/orchestrator/orchestrator.go`), guarded by the existing mutex.
- [x] Toggle `pollChecking` around the tracker poll in the orchestrator's tick loop. Update `lastPollAt` after each poll.
- [x] Populate `Polling` in `BuildSnapshot` (`go/internal/orchestrator/snapshot.go`).
- [x] Add `formatNextPoll(ms int) string` helper to `go/internal/web/funcs.go`. Format: `"Next poll in 1.2s"` when ms > 0, `"Checking…"` when checking. Take a struct or two args.
- [x] Update `go/internal/web/templates/dashboard.html.tmpl` `header-status` sub-template to render the polling state.
- [x] Update `go/internal/observability/testdata/snapshot.golden.json` with the new field. Use a fixed value like `polling: {checking: false, poll_interval_ms: 2000, next_poll_in_ms: 1500}`.
- [x] Add `TestBuildSnapshot_Polling` to `go/internal/orchestrator/snapshot_test.go`.
- [x] Append to `go/SPEC.md` §14.3 documenting the new field.
- [x] Run `cd go && go test ./... -count=1 && go vet ./...` and confirm clean.
- [x] Run `touch openspec-change/.symphony-done` from the workspace root.
