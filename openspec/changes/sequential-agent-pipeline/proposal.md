---
labels:
  - go
  - agent
  - orchestration
  - multi-agent
issues:
  - "follow-up to #2-#6 bundle"
---

# Sequential agent pipeline (planner → implementer → reviewer)

## Why

Today every issue runs through one `agent.Runtime` (codex OR claude OR mock)
that loops `max_turns` times in a single workspace. There's no way to express
"have a planner draft the approach first, then an implementer execute it,
then a reviewer check the result." Real multi-agent orchestration starts here:
a single issue, multiple specialized agents, structured handoff.

This is the cheapest path to actual multi-agent collaboration without
introducing concurrent subagent supervision, message buses, or distributed
coordination. The shared workspace is the medium of exchange — agents
communicate by writing files the next agent reads.

**Implementation language:** Go only. Leaves the Elixir reference untouched.

## What changes

### Configuration: `agent.pipeline`

WORKFLOW.md's existing `agent` block grows an optional `pipeline` list.
When present, it overrides the single-role `runtime` / `max_turns`
behavior. When absent, behavior is byte-identical to today (full backwards
compatibility).

```yaml
agent:
  max_concurrent_agents: 5
  pipeline:
    - role: planner
      runtime: claude
      max_turns: 1
      prompt_template: |
        You are the planner. Read the issue description and produce
        `.symphony/plan.md` in the workspace describing the approach,
        files to change, and acceptance checks. Stop after writing the
        plan.
      ready_artifact: .symphony/plan.md
      timeout_ms: 600000

    - role: implementer
      runtime: codex
      max_turns: 10
      prompt_template: |
        Read .symphony/plan.md and execute it. Write
        .symphony/implementation-summary.md when done.
      ready_artifact: .symphony/implementation-summary.md
      timeout_ms: 3600000

    - role: reviewer
      runtime: claude
      max_turns: 1
      prompt_template: |
        Read .symphony/plan.md and .symphony/implementation-summary.md.
        Post a review comment on the tracker. If satisfied, write
        .symphony/review-approved.md; otherwise write
        .symphony/review-needs-changes.md.
      ready_artifact: .symphony/review-approved.md
      on_artifact:
        ".symphony/review-needs-changes.md":
          retry_from: implementer
          max_loopbacks: 2
```

Per-role fields:

| Field             | Required | Description                                                                 |
| ----------------- | -------- | --------------------------------------------------------------------------- |
| `role`            | yes      | Lowercase identifier; unique within the pipeline.                           |
| `runtime`         | yes      | One of `codex`, `claude`, `mock`. Each role gets its own runtime instance. |
| `max_turns`       | yes      | Per-role turn cap. The orchestrator's existing turn-loop runs against this. |
| `prompt_template` | yes      | Liquid template; receives `issue` + `artifacts` (parsed from previous roles' `ready_artifact` files when present). |
| `ready_artifact`  | yes      | Workspace-relative path. The role completes successfully when this file exists at end-of-turn OR when the agent emits `EventTurnCompleted` AND the file exists. |
| `timeout_ms`      | no       | Wall-clock cap for this role. Default = `agent.max_turns × codex.turn_timeout_ms` (rough upper bound).      |
| `on_artifact`     | no       | Map of artifact path → `{retry_from: <role>, max_loopbacks: N}`. Lets a reviewer loop back to the implementer. |

### Domain: `PipelineProgress`

`domain.Issue` gains an optional `Pipeline` field tracking per-role
completion:

```go
type PipelineProgress struct {
    CurrentRole   string             // currently-running role, "" if done/idle
    CompletedRoles []string           // ordered, e.g. ["planner", "implementer"]
    Loopbacks     map[string]int     // role -> loopback count (capped by max_loopbacks)
    Artifacts     map[string]string  // role -> ready_artifact path resolved at completion
}

type Issue struct {
    // ... existing fields ...
    Pipeline *PipelineProgress `json:"pipeline,omitempty"`
}
```

### Orchestrator: `dispatchPipeline`

A new `dispatchPipeline` path runs alongside the existing `dispatchOne`:

- When `cfg.Agent.Pipeline` is empty, the existing `dispatchOne` runs. No
  behavior change.
- When `cfg.Agent.Pipeline` is non-empty, `dispatchPipeline` iterates the
  roles, building a `agent.Runtime` per role at the start of dispatch (one
  pool, lifetime-of-dispatch). For each role:
  1. Build the prompt by rendering `prompt_template` against the issue +
     artifact map collected so far.
  2. Run the existing turn-loop with that role's `runtime` and `max_turns`.
  3. After the loop returns, check `ready_artifact`. If present, mark the
     role complete and advance. If absent and the loop ended because of
     `max_turns`, treat as failure (schedule retry per existing semantics).
  4. After every successful role, scan `on_artifact` for any artifact the
     agent wrote. If a `retry_from` rule fires, jump back to that role and
     increment its loopback counter. Reject the loop if the role's
     `max_loopbacks` is exceeded.
- The pipeline's progress (`CurrentRole`, `CompletedRoles`, `Loopbacks`,
  `Artifacts`) is written to the running entry on every transition and
  surfaced in the snapshot. Persistence is delegated to the
  `durable-state-json` change so a restart resumes from the correct role
  rather than restarting from `planner`.

### Snapshot / dashboard

`observability.RunningEntry` gains:

```go
PipelineRole       *string  `json:"pipeline_role,omitempty"`
PipelineCompleted  []string `json:"pipeline_completed,omitempty"`
PipelineTotalRoles int      `json:"pipeline_total_roles,omitempty"`
```

The "Running sessions" table grows a "Role" column showing
`<current_role> (i/N)`. The Kanban "In Progress" cards show the same
chip. No structural CSS rework.

### Agent runtime factory

`agent.New` already takes a `config.Config` and dispatches by
`cfg.Agent.Runtime`. Pipeline mode requires building a runtime per role.
Add `agent.NewForRole(cfg config.Config, role config.PipelineRole) (Runtime, error)`
that overrides `cfg.Agent.Runtime` and the `max_turns` on the input config
and delegates to the existing `New`. The orchestrator constructs one map
of `role → Runtime` at orchestrator startup and reuses across dispatches.

### Spec updates

- `go/SPEC.md` §10 (Agent Runner Protocol) gains §10.9 "Pipeline mode" —
  defines the `agent.pipeline` config schema, role advancement semantics,
  artifact handoff, and loopback rules.
- §13 (Snapshot) documents the three new `RunningEntry` fields.
- §14.3 (Snapshot data contract) lists `pipeline_role`,
  `pipeline_completed`, `pipeline_total_roles`.

## Acceptance

- A WORKFLOW.md with no `pipeline:` field still runs every existing test
  unchanged. (Backwards-compatibility regression suite.)
- A WORKFLOW.md with `[planner, implementer, reviewer]` against the mock
  runtime — where each mock is canned to write its `ready_artifact` —
  drives one issue through all three roles, in order, and the dashboard
  shows the role advancing.
- If the planner doesn't write `ready_artifact` after `max_turns`, the
  orchestrator schedules a retry of the **planner** (not the
  implementer).
- A reviewer that writes `.symphony/review-needs-changes.md` causes the
  pipeline to loop back to the implementer; after `max_loopbacks=2`
  re-entries, the dispatch fails.
- Mixing runtimes works: planner=claude, implementer=codex, reviewer=claude.

## Out of scope

- Parallel fan-out / fan-in. Roles execute strictly sequentially.
- Subagent delegation (an agent spawning child agents). Each role is one
  flat agent run.
- Cross-issue coordination. Each issue's pipeline runs independently.
- Cost accounting per role (tracked under the global token totals; per-role
  breakdown is a follow-up).
- Pipeline editing from the dashboard UI. WORKFLOW.md is still the source of
  truth.

## Completion signal

`touch openspec/changes/sequential-agent-pipeline/.symphony-done` when
the acceptance suite passes.
