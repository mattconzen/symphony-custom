# Tasks

- [x] Add `WorkspaceRoot() string` to the orchestrator (returns `o.cfg.Workspace.Root`).
- [x] Extend the small interface used by `internal/web/handlers.go` to include `WorkspaceRoot() string`.
- [x] In `internal/web/api.go`, update the per-issue workspace path builder: use the entry's path if non-empty, else synthesize from root + sanitized identifier.
- [x] Use the same sanitization as `domain.Issue.WorkspaceKey()` (preferably by exporting/calling that method). If exposing it requires an import that creates a cycle, duplicate the regex with a comment pointing at the source.
- [x] Update existing tests that asserted `workspace.path: null` for known identifiers.
- [x] Update SPEC §14.2 to remove the v0 deviation language.
- [x] Run tests + vet, confirm clean.
- [x] `touch openspec-change/.symphony-done`.
