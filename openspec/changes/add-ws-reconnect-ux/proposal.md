---
labels: [dashboard, ux, robustness]
---

# Surface WebSocket reconnect status in the dashboard

## Why

When the WebSocket drops (orchestrator restart, network blip, sandbox reaping), the htmx-ws
extension reconnects silently — but the dashboard has no visible indicator. Operators see a
frozen page with no way to tell whether they're disconnected. Phoenix LiveView gets visible
"connecting…" UX for free; the Go dashboard has to opt in.

## What changes

1. In `dashboard.html.tmpl`, add a small status pill near the existing `header-status`
   container, with explicit ids the htmx-ws extension fires events at:

   ```html
   <span id="ws-status" class="ws-status ws-status-live" data-state="live">
     <span class="ws-status-dot"></span>
     <span class="ws-status-label">Live</span>
   </span>
   ```

2. Add a small inline `<script>` (or a separate `static/dashboard.js` if cleaner — but inline
   is fine for ~20 lines) that listens for htmx-ws lifecycle events:
   - `htmx:wsOpen` → set state to `live`, label "Live", green dot.
   - `htmx:wsClose` → set state to `disconnected`, label "Reconnecting…", amber dot, animated.
   - `htmx:wsError` → same as Close but red dot, label "Connection error".
   - `htmx:wsConnecting` → state `connecting`, label "Connecting…".

3. Add CSS for `.ws-status-live` (green), `.ws-status-disconnected` (amber, pulse animation),
   `.ws-status-connecting` (grey), `.ws-status-error` (red) to `dashboard.css`.

4. Note: the htmx-ws extension auto-reconnects with exponential backoff by default. We don't
   need to implement reconnect logic; we just surface the events.

## Acceptance

- Visible pill on the dashboard near the header.
- Manual smoke: kill the symphony binary while the dashboard is open; the pill goes amber
  with "Reconnecting…". Restart symphony; the pill goes green ("Live") within ~2s.
- Test in `handler_test.go` confirming the pill HTML (with `id="ws-status"`) is in `GET /`
  output.

## Spec update

Append a brief paragraph to SPEC §14.4 (Real-Time Update Protocol) describing the visible
states and that they're driven by htmx-ws lifecycle events.

## Completion signal

`touch openspec-change/.symphony-done`.
