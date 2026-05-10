# Tasks

## 1. New package: `internal/transcript/`

- [ ] `Writer` type with `Append(ev Event)` (atomic-per-line append) and `Close()`.
- [ ] `Reader` type with `Read(from int, limit int) ([]Event, int, error)` (streams the file, returns next `from`).
- [ ] `Bus` type with `Subscribe(issueIdentifier string)` returning a per-subscriber buffered channel + unsubscribe func; `Publish(ev Event)` non-blocking fan-out.
- [ ] 64KB payload truncation logic; emits `Truncated: true` flag.

## 2. Orchestrator integration

- [ ] In `dispatchOne` (and `dispatchPipeline` from sibling change), open a `transcript.Writer` for the dispatch's workspace at start, close on dispatch finish.
- [ ] In the existing event callback, mirror every `agent.Event` to the writer + `transcript.Bus.Publish`.
- [ ] Populate `agent_session_logs` on the per-issue JSON with the tail (last N) — read on demand via the reader, not cached on the running entry.

## 3. New endpoint: GET /api/v1/issues/{id}/transcript

- [ ] In `internal/web/`, add the handler. Read pagination params (`from`, `limit` with max 1000).
- [ ] Resolve the JSONL path from the issue's workspace key (use existing `domain.Issue.WorkspaceKey()` + workspace root).
- [ ] Return `{identifier, events, next_from, complete}` JSON.
- [ ] 404 if no transcript file exists yet.

## 4. WS subscribe / unsubscribe protocol

- [ ] Extend `internal/web/ws.go` to read inbound text messages on the existing `/ws` connection.
- [ ] Decode `{subscribe_transcript: "WEB-1"}` and `{unsubscribe_transcript: "WEB-1"}` envelopes; track a per-connection set.
- [ ] On `transcript.Bus` publish, walk subscribers and send `<div hx-swap-oob="beforeend:#transcript-{id}">…</div>` fragments.
- [ ] Apply the same non-blocking-send / drop-on-full-buffer policy as the existing dashboard broadcaster.

## 5. /issue/{id} page integration

- [ ] Add the "Transcript" section to `templates/issue.html.tmpl`. Server-side render reads the most recent 200 events.
- [ ] Add a small inline JS that:
    - Sends `subscribe_transcript` after the page's WS opens.
    - Toggles a "Pause live" / "Resume live" button (client-side gate, doesn't unsubscribe).
    - "Clear" empties the rendered list (visual only — file untouched).
- [ ] CSS rules in `internal/web/static/dashboard.css` for the transcript list (monospace, kind-color, truncation).

## 6. Tests

- [ ] `internal/transcript/`: writer round-trip, reader pagination, bus fan-out, truncation, concurrent appends.
- [ ] `internal/web/`: handler tests for the new endpoint; subscribe / publish / receive over a `httptest`-ed WS.
- [ ] E2E: dispatch a mock issue, assert the transcript file exists with the expected event kinds.

## 7. Docs

- [ ] `go/SPEC.md` §13.4 + §14.2 + §14.4 updates.
- [ ] `go/README.md` operator note pointing at `<workspace>/.symphony/transcript.jsonl` for grep / jq.

## 8. Completion

- [ ] `touch openspec/changes/agent-transcript-viewer/.symphony-done`.
