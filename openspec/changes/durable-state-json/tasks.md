# Tasks

## 1. New package: `internal/durable/`

- [x] Define `type Store struct { path string; mu map[string]*sync.Mutex; ... }`.
- [x] `New(path string) (*Store, error)` — creates path if missing; verifies write access.
- [x] `Save(name string, payload any) error` — atomic write via `<path>/<name>.tmp.<rand>` + `os.Rename`. fsync the file before rename.
- [x] `Load(name string, into any) error` — `os.ReadFile` + `json.Unmarshal`. Returns `os.ErrNotExist` cleanly so callers can fall back to seed.
- [x] `Delete(name string) error` — for spec-job cleanup.
- [x] Per-name `sync.Mutex` to serialize concurrent saves to the same file.
- [x] Schema-version helper: every write wraps payload in `{"version":<int>,"data":<payload>}`.

## 2. Config

- [x] Add `Durable struct { Path string; Enabled bool }` to `internal/config/config.go`.
- [x] Parse `durable:` block from WORKFLOW.md. Default: `Enabled=true`, `Path=./.symphony/state` resolved relative to workflow dir.
- [x] Preflight: when `Enabled`, ensure path is writable.

## 3. Orchestrator integration

- [x] Add `WithDurableStore(s *durable.Store) *Orchestrator`.
- [x] In `Run`, load `orchestrator.json`, `pipelines.json`, `spec_jobs.json` before the first tick. Schema mismatch → return error.
- [x] On every `notify()`, mark `orchestrator.json` dirty.
- [x] Background goroutine: 200ms debounce, write on dirty.
- [x] On `ctx.Done()`, force a synchronous write before returning.

## 4. Memory tracker integration

- [x] Add `WithDurableStore(s *durable.Store) *MemoryTracker`.
- [x] On `New`, attempt `Load("trackers/memory_issues")` + `Load("trackers/memory_specs")`; fall back to seed when missing.
- [x] On `CreateIssue` / `UpdateIssueState` / `WriteSpec` / `SetPullRequest`, write through after the in-memory mutation.
- [x] Handle the load-newer-than-supported case explicitly.

## 5. Web handler integration

- [x] Add `WithDurableStore(s *durable.Store)` to the web handler.
- [x] On startup, load `spec_jobs.json` and mark any non-`done` jobs as `error: "interrupted by restart"`.
- [x] On every state change of a `specJob`, persist the slim form (identifier, job_id, status, started_at, body, error) — NOT the in-flight goroutine state.

## 6. main.go wiring

- [x] Construct `durable.Store` when `cfg.Durable.Enabled`. Pass to orchestrator + memory tracker (when active) + web handler.
- [x] Log the durable path on startup.

## 7. Tests

- [x] `internal/durable/`: round-trip (Save → Load), atomic-rename (interrupted write doesn't corrupt), schema-version mismatch returns sentinel error.
- [x] `internal/orchestrator/`: dispatch → cancel ctx → re-construct orchestrator with same durable path → retry queue + PR cache restored.
- [x] `internal/tracker/memory/`: Create + Restart cycle with same durable path; new MemoryTracker exposes the previously created issues.
- [x] `go/e2e/durable_restart_test.go`: end-to-end as in the proposal acceptance.

## 8. Docs

- [x] `go/SPEC.md` §6 (config block) and §15.5 (durable state semantics).
- [x] `go/README.md` example showing `durable:` block + path.

## 9. Completion

- [x] `touch openspec/changes/durable-state-json/.symphony-done`.
