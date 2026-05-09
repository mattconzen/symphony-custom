---
labels: [dashboard, deviation-fix]
---

# Synthesize `workspace.path` for issues without an active workspace

## Why

`GET /api/v1/{id}` currently returns `workspace.path: null` whenever neither the running nor
retrying entry carries one. SPEC §14.2 documents this as a v0 deviation — Elixir synthesizes
`<workflow_root>/<sanitized_id>` instead. Honoring the same convention removes a parity
deviation and gives consumers a usable path to point operators at, even for issues that
haven't been dispatched yet.

## What changes

1. Pass the workspace root into the web handler. `internal/web/handlers.go`'s `NewHandler`
   gains a small interface or extends the existing `snapshotSource` to expose
   `WorkspaceRoot() string`. Implementation on `*orchestrator.Orchestrator`: `return
   o.cfg.Workspace.Root`.

2. Update `internal/web/api.go`'s `buildWorkspace` (or whatever helper produces the
   `workspace` slot of the per-issue payload):
   - If running/retrying entry has a non-empty path → use it.
   - Otherwise → synthesize `filepath.Join(workspaceRoot, sanitize(identifier))` using the
     same sanitization rule as `domain.Issue.WorkspaceKey()` — preferably by exposing that
     method or duplicating the regex.

3. Update SPEC §14.2 to remove the "v0 deviation" language about `workspace.path: null`. New
   wording: "`workspace.path` is the resolved or synthesized workspace directory for the
   issue; consumers MUST treat it as the canonical path even when no dispatch has yet created
   the directory on disk."

## Acceptance

- New test in `internal/web/api_test.go`: a stub orchestrator with no running or retrying
  entries for a known identifier returns `workspace.path = "/configured/root/<id>"`.
- Existing test that checks `null` is updated (or removed). The `null` case is no longer
  reachable for any seeded identifier; only truly unknown identifiers (which return 404) hit
  the absence path.
- E2E `web_test.go` may need a small adjustment if it asserts on `null`.
- `cd go && go test ./... -count=1 && go vet ./...` clean.

## Spec update

Per the rewording above.

## Completion signal

`touch openspec-change/.symphony-done`.
