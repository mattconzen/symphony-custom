# Tasks

Implement in this order — each phase is independently testable.

## 1. Config schema

- [x] Add `PipelineRole` and `PipelineLoopback` types to `internal/config/config.go`.
- [x] Parse `agent.pipeline` from WORKFLOW.md front-matter; populate `cfg.Agent.Pipeline []PipelineRole`.
- [x] Validate in `Preflight`: role names unique, `runtime` is valid, `prompt_template` non-empty, `ready_artifact` non-empty, `on_artifact[*].retry_from` references an earlier role.
- [x] Backwards-compat: when `pipeline` is absent, `cfg.Agent.Pipeline` is nil and existing single-role code path runs.

## 2. Domain

- [x] Add `PipelineProgress` struct to `internal/domain/issue.go`.
- [x] Add `Pipeline *PipelineProgress` field to `domain.Issue`.

## 3. Agent factory per role

- [x] Add `agent.NewForRole(cfg config.Config, role config.PipelineRole) (Runtime, error)` that produces a runtime configured per the role's `runtime` + per-role overrides.
- [x] Update orchestrator to maintain a `roleRuntimes map[string]agent.Runtime` built at orchestrator startup.

## 4. Pipeline dispatch

- [x] Add `internal/orchestrator/pipeline.go` with `dispatchPipeline(ctx, issue)` that:
    - Iterates `cfg.Agent.Pipeline` in order, starting from `issue.Pipeline.CurrentRole` if set (resume), else from index 0.
    - For each role, renders the per-role prompt and runs the existing turn-loop with that role's runtime + `max_turns`.
    - After turn loop, checks `ready_artifact` existence; if absent → schedule retry against the same role.
    - After success, scans `on_artifact` map; if a hit and within `max_loopbacks`, sets `CurrentRole` back to the named role and re-enters that role.
    - Updates `Issue.Pipeline` (running entry + tracker, if `tracker.PullRequestSetter`-style optional interface implemented) on every transition. Calls `o.notify()` on every transition.
- [x] In `dispatchOne`, branch on `cfg.Agent.Pipeline`: empty → existing path, non-empty → call `dispatchPipeline`.

## 5. Prompt rendering

- [x] Extend `internal/prompt/render.go` (or wherever Liquid rendering lives) so role prompt templates receive `{ issue, artifacts }`.
- [x] Build `artifacts` by reading every previous role's `ready_artifact` file from the workspace and stuffing the contents into a map keyed by role name.

## 6. Snapshot + UI

- [x] Add `PipelineRole`, `PipelineCompleted`, `PipelineTotalRoles` to `observability.RunningEntry`.
- [x] Populate from the running entry's `domain.Issue.Pipeline`.
- [x] Update `dashboard.html.tmpl` running-sessions table: add a "Role" column. Also annotate Kanban "In Progress" cards with the role chip.
- [x] Update SPEC §13 / §14.3.

## 7. Tests

- [x] Unit: `Preflight` rejects malformed pipelines (duplicate roles, unknown runtime, bad `retry_from` reference).
- [x] Unit: `dispatchPipeline` orchestration with a fake runtime that writes the ready_artifact when expected; verify advancement order.
- [x] Unit: failing-planner schedules a retry of the planner (not the next role).
- [x] Unit: reviewer-rejection loop respects `max_loopbacks`.
- [x] E2E (`go/e2e/`): seed an issue, run a 3-role mock pipeline end-to-end, verify dashboard reflects role transitions over the WS.

## 8. Docs

- [x] `go/SPEC.md` §10.9.
- [x] `go/README.md` example WORKFLOW.md with a pipeline.

## 9. Completion

- [x] `touch openspec/changes/sequential-agent-pipeline/.symphony-done`.
