# Tasks

- [x] Add `<span id="ws-status" class="ws-status ws-status-live">…</span>` near `header-status` in `dashboard.html.tmpl`. Place it OUTSIDE the `header-status` OOB-swap container so WebSocket fragment pushes don't replace it.
- [x] Add inline `<script>` (or `static/dashboard.js` + script tag) that listens for `htmx:wsOpen`, `htmx:wsClose`, `htmx:wsError`, `htmx:wsConnecting`. The script mutates the pill's class and label.
- [x] Add CSS rules for `.ws-status` and the four state variants in `static/dashboard.css`. Use a subtle pulse animation for the disconnected state.
- [x] Add `TestDashboard_WSStatusPill` to `handler_test.go` asserting the pill HTML is present.
- [x] Manual smoke: kill+restart symphony, observe pill state changes.
- [x] Append to SPEC §14.4 a paragraph documenting the visible states.
- [x] Run tests + vet + build, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
