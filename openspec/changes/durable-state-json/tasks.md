# Tasks

## 1. New package: `internal/durable/`

- [ ] Define `type Store struct { path string; mu map[string]*sync.Mutex; ... }`.
- [ ] `New(path string) (*Store, error)` — creates path if missing; verifies write access.
- [ ] `Save(name string, payload any) error` — atomic write via `<path>/<name>.tmp.<rand>` + `os.Rename`. fsync the file before rename.
- [ ] `Load(name string, into any) error` — `os.ReadFile` + `json.Unmarshal`. Returns `os.ErrNotExist` cleanly so callers can fall back to seed.
- [ ] `Delete(name string) error` — for spec-job cleanup.
- [ ] Per-name `sync.Mutex` to serialize concurrent saves to the same file.
- [ ] Schema-version helper: every write wraps payload in `{"version":<int>,"data":<payload>}`.

## 2. Config

- [ ] Add `Durable struct { Path string; Enabled bool }` to `internal/config/config.go`.
- [ ] Parse `durable:` block from WORKFLOW.md. Default: `Enabled=true`, `Path=./.symphony/state` resolved relative to workflow dir.
- [ ] Preflight: when `Enabled`, ensure path is writable.

## 3. Orchestrator integration

- [ ] Add `WithDurableStore(s *durable.Store) *Orchestrator`.
- [ ] In `Run`, load `orchestrator.json`, `pipelines.json`, `spec_jobs.json` before the first tick. Schema mismatch → return error.
- [ ] On every `notify()`, mark `orchestrator.json` dirty.
- [ ] Background goroutine: 200ms debounce, write on dirty.
- [ ] On `ctx.Done()`, force a synchronous write before returning.

## 4. Memory tracker integration

- [ ] Add `WithDurableStore(s *durable.Store) *MemoryTracker`.
- [ ] On `New`, attempt `Load("trackers/memory_issues")` + `Load("trackers/memory_specs")`; fall back to seed when missing.
- [ ] On `CreateIssue` / `UpdateIssueState` / `WriteSpec` / `SetPullRequest`, write through after the in-memory mutation.
- [ ] Handle the load-newer-than-supported case explicitly.

## 5. Web handler integration

- [ ] Add `WithDurableStore(s *durable.Store)` to the web handler.
- [ ] On startup, load `spec_jobs.json` and mark any non-`done` jobs as `error: "interrupted by restart"`.
- [ ] On every state change of a `specJob`, persist the slim form (identifier, job_id, status, started_at, body, error) — NOT the in-flight goroutine state.

## 6. main.go wiring

- [ ] Construct `durable.Store` when `cfg.Durable.Enabled`. Pass to orchestrator + memory tracker (when active) + web handler.
- [ ] Log the durable path on startup.

## 7. Tests

- [ ] `internal/durable/`: round-trip (Save → Load), atomic-rename (interrupted write doesn't corrupt), schema-version mismatch returns sentinel error.
- [ ] `internal/orchestrator/`: dispatch → cancel ctx → re-construct orchestrator with same durable path → retry queue + PR cache restored.
- [ ] `internal/tracker/memory/`: Create + Restart cycle with same durable path; new MemoryTracker exposes the previously created issues.
- [ ] `go/e2e/durable_restart_test.go`: end-to-end as in the proposal acceptance.

## 8. Docs

- [ ] `go/SPEC.md` §6 (config block) and §15.5 (durable state semantics).
- [ ] `go/README.md` example showing `durable:` block + path.

## 9. Completion

- [ ] `touch openspec/changes/durable-state-json/.symphony-done`.
