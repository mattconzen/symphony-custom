# Tasks

- [x] Add `handleHealthz` to `internal/web/handlers.go`. Returns 200 + `ok\n` when alive and the orchestrator has polled at least once; 503 + `starting\n` otherwise.
- [x] Add `handleMetrics` to `internal/web/handlers.go`. Pull values from `orch.Snapshot()`; emit Prometheus text format manually (no client library dep). Include HELP and TYPE comments for every series.
- [x] Series to emit: `symphony_running_sessions` (gauge), `symphony_retrying_sessions` (gauge), `symphony_codex_tokens_total{type=...}` (counter), `symphony_codex_seconds_running` (counter), `symphony_polling_checking` (gauge), `symphony_polling_interval_ms` (gauge).
- [x] Register both routes in `NewHandler`. Method-not-allowed for non-GET returns 405.
- [x] Add `TestHealthz` (200 normal path, optional 503 startup path if mockable).
- [x] Add `TestMetrics_Format` asserting all series names + HELP/TYPE comments.
- [x] Update SPEC §14.2 routes table with the two new endpoints.
- [x] Add SPEC §14.6 (Operations Endpoints) documenting the metric names and types.
- [x] Run tests + vet + build, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
