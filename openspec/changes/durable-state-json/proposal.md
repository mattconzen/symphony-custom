---
labels:
  - go
  - persistence
  - durability
issues:
  - "follow-up to #2-#6 bundle"
---

# Durable orchestrator state on disk (JSON files)

## Why

Today every piece of orchestrator state is in-process:

| What                                | Where                                       | On restart |
| ----------------------------------- | ------------------------------------------- | ---------- |
| Running issues                      | `Orchestrator.running map`                  | Lost       |
| Retry queue                         | `Orchestrator.retryAttempts map`            | Lost       |
| PR cache                            | `Orchestrator.pullRequests map`             | Lost       |
| Memory tracker issues               | `MemoryTracker.issues slice`                | Lost       |
| Memory tracker specs                | `MemoryTracker.specs map`                   | Lost       |
| Spec-generation jobs                | `Handler.specJobs map`                      | Lost       |
| Pipeline progress (from sibling change) | per-issue domain field, in-memory only  | Lost       |

For a real multi-agent operator, restart = lose every in-flight retry
timer, the current pipeline role for every running issue, the merge state
of every tracked PR, and (for memory-tracker users) every issue ever
created from the dashboard. This makes Symphony brittle: an OOM, a
deploy, or a routine restart costs the operator their context.

## What changes

A new `internal/durable/` package writes a small set of JSON files to a
configurable directory and reloads them on startup. No database, no
external service — atomic-write + rename, schema-versioned.

### Configuration

WORKFLOW.md gains:

```yaml
durable:
  path: ./.symphony/state         # default. Relative paths resolve against the workflow dir.
  enabled: true                   # default true; false reverts to in-memory only.
```

When `enabled: false`, behavior is byte-identical to today.

### File layout

```
<durable.path>/
  schema.json                     # { "version": 1, "written_at": "..." }
  orchestrator.json               # running, retrying, claims, agentTotals, pullRequests, kanban caches
  pipelines.json                  # per-issue PipelineProgress (from sibling change)
  spec_jobs.json                  # generation jobs the dashboard hands out
  trackers/
    memory_issues.json            # MemoryTracker.issues (when active tracker is "memory")
    memory_specs.json             # MemoryTracker.specs
```

Each file's top-level shape is `{ "version": <int>, "data": {...} }`. The
loader rejects mismatched versions on startup (operator-driven migration
expected; out of scope to auto-migrate).

### Atomic writes

`durable.Save(name string, payload any)` writes
`<path>/<name>.tmp.<rand>`, fsyncs, and renames over `<path>/<name>`.
Concurrent saves to the same name are serialized via a per-file mutex.
Reads are lock-free `os.ReadFile`.

### Snapshot strategy: write-on-notify, with debounce

The orchestrator already calls `o.notify()` on every observable
transition. The durable writer subscribes to that callback and:

- Marks `orchestrator.json` dirty on any notify.
- Coalesces writes via a 200ms debounce timer so a burst of transitions
  produces one file write.
- Writes synchronously on `ctx.Done()` so a graceful shutdown captures
  the latest state before the process exits.

Per-tracker state (memory tracker issues + specs) is written on every
mutating call (`CreateIssue`, `UpdateIssueState`, `WriteSpec`,
`SetPullRequest`) before the call returns, so a crash mid-flight at
worst loses the *current* call.

### Load sequence

`Orchestrator.New` accepts an optional `durable.Store`. When non-nil,
`Run(ctx)` opens the store, loads `orchestrator.json` + `pipelines.json`
+ `spec_jobs.json` + (for memory tracker) `memory_*.json`, and seeds the
in-memory state from them. If the schema version doesn't match, `Run`
returns an explicit error rather than silently truncating.

### Failure isolation

The durable layer is best-effort within the runtime: a write error logs
a warning but does not abort the orchestrator. A *load* error at startup
is fatal — operators should never silently lose state.

### Memory-tracker specifics

`memory.MemoryTracker` gets an optional `durable.Store` field. When set:

- `New(seed)` first attempts to load `memory_issues.json` + `memory_specs.json` from the store; if either is missing or empty, falls back to `seed`.
- `CreateIssue` / `UpdateIssueState` / `WriteSpec` / `SetPullRequest` write through to the store before returning.

This makes the memory tracker durable enough for a single-machine
operator who doesn't want a real backend.

### Spec-generation jobs

`Handler.specJobs` migrates from a `map[string]*specJob` to a
`durable.Store`-backed map. In-flight job state (the goroutine running
the agent) is *not* persisted; on restart, pending jobs are marked
`error: "interrupted by restart"` and the operator can re-trigger.

### Spec updates

- `go/SPEC.md` §15 (Failure Model) gains §15.5 "Durable State" describing
  the schema, file layout, and load semantics.
- §6 (Configuration) documents the `durable:` block.
- A migration policy doc: schema versions are integers, bumped only on
  breaking changes; loaders refuse to read newer versions.

## Acceptance

- `cd go && go test ./internal/durable/... -count=1` passes.
- A new e2e test (`go/e2e/durable_restart_test.go`):
  1. Starts an orchestrator + memory tracker + durable store under a
     `t.TempDir()`.
  2. Seeds an issue, dispatches it, schedules a retry.
  3. Cancels the orchestrator context.
  4. Constructs a fresh orchestrator pointing at the same durable
     directory.
  5. Asserts the retry queue, PR cache, and memory issues are all
     restored.
- Killing `symphony` mid-run with SIGKILL and restarting it on the same
  durable path resumes the running snapshot from disk (PR cache + memory
  issues + retry queue intact); pipeline progress (from sibling change)
  resumes from the correct role.
- Disabling `durable.enabled` reverts every test in the existing suite
  to passing identically.

## Out of scope

- Per-issue audit log persistence (handled by the sibling
  `agent-transcript-viewer` change — transcripts persist to JSONL even
  with `durable.enabled: false`).
- Distributed/multi-process state. JSON files have no locking discipline
  beyond the in-process mutex; running two `symphony` processes against
  the same durable path is unsupported.
- SQLite or other DB backends. JSON-on-disk is intentionally minimal.
- Backups, encryption, or rotation. Operators handle that with their
  filesystem of choice.
- Schema migration tooling. v1 → v2 is operator-driven; the loader fails
  fast on mismatch.

## Completion signal

`touch openspec/changes/durable-state-json/.symphony-done`.
