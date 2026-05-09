---
labels: [observability, ops]
---

# Add `/healthz` and `/metrics` endpoints

## Why

The web layer exposes `/api/v1/state` for snapshot consumers but no standardized liveness
(`/healthz`) or scrape (`/metrics`) endpoints. Anyone running symphony in production behind
their existing monitoring stack has to write a custom collector that pulls and parses
`/api/v1/state`. A small `/healthz` plus a Prometheus-format `/metrics` endpoint covers the
common cases.

## What changes

1. **`GET /healthz`**: returns `200 OK` with body `ok\n` whenever the process is alive and
   the orchestrator's poll loop has fired at least once (use the `lastPollAt` field added by
   the polling-snapshot-field change, OR a simpler "is the orchestrator's context alive"
   check if that change isn't yet landed). Returns `503 Service Unavailable` with body
   `starting\n` during the brief startup window before the first poll. No JSON, no auth —
   the simplest possible liveness probe.

2. **`GET /metrics`**: returns Prometheus text format with at least these series:
   - `symphony_running_sessions` (gauge): current count of running issues.
   - `symphony_retrying_sessions` (gauge): current retry queue depth.
   - `symphony_codex_tokens_total{type="input"}` (counter): total input tokens.
   - `symphony_codex_tokens_total{type="output"}` (counter): total output tokens.
   - `symphony_codex_seconds_running` (counter): cumulative runtime seconds.
   - `symphony_polling_checking` (gauge, 0 or 1): is a poll in flight.
   - `symphony_polling_interval_ms` (gauge): configured poll interval.

   Implementation: pull values from the snapshot via `orch.Snapshot()`. No external
   dependency on the Prometheus client library — emit the text format manually (it's
   ~30 lines of `fmt.Fprintf`). HELP and TYPE comments are required by the format.

3. Both endpoints are registered in `internal/web/handlers.go`'s `NewHandler`. Method-not-
   allowed for non-GET returns 405 (consistent with the existing API).

## Acceptance

- New tests in `handler_test.go`: `TestHealthz`, `TestMetrics_Format`. `TestMetrics_Format`
  asserts the response contains all the documented series names with expected types and
  HELP comments.
- Manual smoke: `curl http://127.0.0.1:4000/healthz` returns 200, `curl
  http://127.0.0.1:4000/metrics` returns text/plain with all series.
- `cd go && go test ./... -count=1 && go vet ./...` clean.

## Spec update

Append to SPEC §14.2 the two new routes:
- `GET /healthz` — liveness probe; 200 or 503 plain text.
- `GET /metrics` — Prometheus exposition format; 200 always.

Add a §14.6 (Operations Endpoints) documenting the metric names and types as a stable
contract.

## Completion signal

`touch openspec-change/.symphony-done`.
