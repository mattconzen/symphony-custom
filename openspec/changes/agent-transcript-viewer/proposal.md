---
labels:
  - go
  - dashboard
  - observability
  - audit
issues:
  - "follow-up to #2-#6 bundle"
---

# Agent transcript: JSONL on disk + WebSocket live tail

## Why

The dashboard surfaces token totals, last event, and last message — but
operators have no way to answer "what did the agent actually *do* on this
issue?" The per-issue JSON includes an `agent_session_logs: []`
placeholder that's never populated. There's no audit trail, no
post-mortem, no debugging surface.

For multi-agent orchestration this is even more painful: with the
sequential pipeline change, an issue's history spans multiple roles
(planner → implementer → reviewer), and operators need to see each
agent's reasoning to figure out where things went wrong.

## What changes

Every `agent.Event` emitted during a dispatch is appended to a
per-issue JSONL file on disk. The dashboard's `/issue/{id}` page grows
a new "Transcript" section that shows the file's contents and live-tails
new events over the existing `/ws` WebSocket.

### File layout

For each dispatch, the orchestrator writes to:

```
<workspace_root>/<workspace_key>/.symphony/transcript.jsonl
```

(co-located with the workspace so cleanup / archival works the same way
as the rest of the workspace state, and so multi-machine deployments
naturally separate per-host transcripts).

JSONL line format:

```json
{
  "ts": "2026-05-10T12:00:00.123Z",
  "issue_id": "WEB-1",
  "issue_identifier": "WEB-1",
  "session_id": "sess-abc",
  "role": "implementer",
  "turn": 3,
  "kind": "assistant_message",
  "payload": { ... }
}
```

`role` is empty when the dispatch isn't running under the sequential
pipeline (sibling change). `payload` is the same `any` payload the agent
runtime emits today — no structural change.

Append is atomic-per-line: open with `O_APPEND|O_CREATE`, write a single
serialized line + `\n`. Concurrent appends from multiple turns are safe
on POSIX (single `write` calls under PIPE_BUF, which JSONL events
generally are — large payloads are truncated at 64KB with a `truncated:
true` field). One file handle per dispatch, closed on dispatch finish.

### Orchestrator: transcript writer

A new `internal/transcript/` package provides:

```go
type Writer struct { ... }
func NewWriter(path string) (*Writer, error)
func (w *Writer) Append(ev Event) error
func (w *Writer) Close() error

type Event struct {
    Ts              time.Time `json:"ts"`
    IssueID         string    `json:"issue_id"`
    IssueIdentifier string    `json:"issue_identifier"`
    SessionID       string    `json:"session_id,omitempty"`
    Role            string    `json:"role,omitempty"`
    Turn            int       `json:"turn,omitempty"`
    Kind            string    `json:"kind"`
    Payload         any       `json:"payload,omitempty"`
    Truncated       bool      `json:"truncated,omitempty"`
}
```

The orchestrator's existing `cb` callback in `dispatch.go` (already
scanning for PR links) gains a transcript-write side-effect. The
`agent_session_logs` field on the per-issue JSON gets populated with
the *tail* (last N events, configurable, default 50) so scrapers don't
need to fetch the whole file.

### Reader API

A new endpoint:

```
GET /api/v1/issues/{id}/transcript?from=<seq>&limit=<n>
```

returns:

```json
{
  "identifier": "WEB-1",
  "events": [ ... ],
  "next_from": 124,
  "complete": false
}
```

`from` is a 0-based sequence number (line number in the JSONL file);
`limit` defaults to 200, capped at 1000. The reader streams the file
sequentially — for the volumes Symphony emits per dispatch (hundreds of
events) this is plenty.

### WebSocket live tail

The existing `/ws` already pushes htmx OOB swap fragments scoped to
named region IDs. This change extends that with one new fragment per
issue currently displayed in a transcript view:

- Client subscribes by sending a small JSON message after connect:
  `{"subscribe_transcript": "WEB-1"}`. The handler tracks per-connection
  subscriptions in a map.
- When a new transcript event is appended for a subscribed issue, the
  server pushes:
  `<div hx-swap-oob="beforeend:#transcript-WEB-1"><div class="transcript-event">...</div></div>`
- Unsubscribe by closing the connection or sending
  `{"unsubscribe_transcript": "WEB-1"}`.

A small `internal/transcript.Bus` fans the broadcaster out to per-issue
subscribers; a slow client whose buffer is full drops events for *that
issue* (consistent with the existing dashboard broadcaster's policy).

### `/issue/{id}` page integration

A new "Transcript" section renders below the existing OpenSpec section:

```html
<section id="transcript-WEB-1" class="transcript">
  <header>
    <h2>Transcript</h2>
    <div class="transcript-toolbar">
      <button data-action="transcript-pause">Pause live</button>
      <button data-action="transcript-clear">Clear</button>
    </div>
  </header>
  <ol class="transcript-events">
    {{ range .Transcript.Events }}
      <li class="transcript-event" data-kind="{{ .Kind }}">
        <span class="ts">{{ .Ts }}</span>
        <span class="kind">{{ .Kind }}</span>
        {{ if .Role }}<span class="role">{{ .Role }}</span>{{ end }}
        <pre class="payload">{{ prettyValue .Payload }}</pre>
      </li>
    {{ end }}
  </ol>
</section>
```

Initial render fetches the last 200 events server-side. The page then
opens its own WebSocket subscription (the existing `/ws`) and appends
new lines as they arrive.

A "Pause live" button suspends client-side appending without closing
the WS — the operator can scroll without the view jumping.

### Cleanup

Transcript files live alongside workspaces. The existing workspace
cleanup hook (`before_remove`) sees them automatically. No new
retention policy: if the operator wants to keep transcripts beyond
issue lifetime, they configure it the same way they keep workspaces.

### Spec updates

- `go/SPEC.md` §13 (Logging, Status, Observability) gains §13.4
  "Transcript" describing the file format, append semantics, and 64KB
  payload cap.
- §14.2 (Routes) adds `GET /api/v1/issues/{id}/transcript`.
- §14.4 (Real-time update protocol) documents the new
  `subscribe_transcript` / `unsubscribe_transcript` client messages and
  the transcript OOB-swap fragment ID convention.

## Acceptance

- `cd go && go test ./internal/transcript/... -count=1` passes.
- A dispatch run against the mock runtime appends one JSONL line per
  emitted event. Lines parse as valid JSON.
- `GET /api/v1/issues/{id}/transcript` returns the last 200 events for
  that issue, in chronological order.
- A browser session on `/issue/{id}` sees new events appear in the
  transcript section without reload, within ~250ms of the orchestrator
  emitting them.
- Payloads larger than 64KB are truncated with `truncated: true`; the
  rest of the JSON line is still valid.
- Killing the orchestrator mid-dispatch leaves the JSONL file readable —
  no half-written lines.

## Out of scope

- Transcript storage in a database. JSONL on disk is intentionally
  simple and grep-friendly.
- Cross-issue search ("find every event where the agent ran git push").
  The files are amenable to `jq` and `rg`; no built-in search UI.
- Retention policy / log rotation. Same lifecycle as workspaces.
- Compression. Lines are short; pipe through `gzip` if needed.
- Streaming the transcript over SSE as an alternative to WebSocket.

## Completion signal

`touch openspec/changes/agent-transcript-viewer/.symphony-done`.
