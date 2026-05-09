# Tasks

- [ ] Add `handleHealthz` to `internal/web/handlers.go`. Returns 200 + `ok\n` when alive and the orchestrator has polled at least once; 503 + `starting\n` otherwise.
- [ ] Add `handleMetrics` to `internal/web/handlers.go`. Pull values from `orch.Snapshot()`; emit Prometheus text format manually (no client library dep). Include HELP and TYPE comments for every series.
- [ ] Series to emit: `symphony_running_sessions` (gauge), `symphony_retrying_sessions` (gauge), `symphony_codex_tokens_total{type=...}` (counter), `symphony_codex_seconds_running` (counter), `symphony_polling_checking` (gauge), `symphony_polling_interval_ms` (gauge).
- [ ] Register both routes in `NewHandler`. Method-not-allowed for non-GET returns 405.
- [ ] Add `TestHealthz` (200 normal path, optional 503 startup path if mockable).
- [ ] Add `TestMetrics_Format` asserting all series names + HELP/TYPE comments.
- [ ] Update SPEC §14.2 routes table with the two new endpoints.
- [ ] Add SPEC §14.6 (Operations Endpoints) documenting the metric names and types.
- [ ] Run tests + vet + build, confirm clean.
- [ ] `touch openspec-change/.symphony-done`.
