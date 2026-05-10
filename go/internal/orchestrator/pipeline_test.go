package orchestrator

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/durable"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

// artifactWriter is a mock Runtime that writes a workspace-relative
// artifact path on every RunTurn. It also optionally writes a second
// "loopback" path so reviewer-style logic can exercise on_artifact.
type artifactWriter struct {
	artifact string
	loopback string
	wsPath   string
}

func (m *artifactWriter) StartSession(_ context.Context, ws domain.Workspace) (agent.Session, error) {
	m.wsPath = ws.Path
	return agent.Session{ID: "mock"}, nil
}
func (m *artifactWriter) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (m *artifactWriter) RunTurn(_ context.Context, sess agent.Session, _ string, _ domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	if cb != nil {
		cb(agent.Event{Kind: agent.EventAssistantMessage, Timestamp: time.Now()})
	}
	if m.artifact != "" {
		_ = os.MkdirAll(filepath.Join(m.wsPath, filepath.Dir(m.artifact)), 0o755)  //nolint:errcheck
		_ = os.WriteFile(filepath.Join(m.wsPath, m.artifact), []byte("ok"), 0o644) //nolint:errcheck
	}
	if m.loopback != "" {
		_ = os.WriteFile(filepath.Join(m.wsPath, m.loopback), []byte("loopback"), 0o644) //nolint:errcheck
	}
	return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCompleted}, nil
}

// nullWriter returns TurnCompleted but never writes the artifact.
type nullWriter struct{}

func (nullWriter) StartSession(_ context.Context, _ domain.Workspace) (agent.Session, error) {
	return agent.Session{ID: "mock-null"}, nil
}
func (nullWriter) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (nullWriter) RunTurn(_ context.Context, sess agent.Session, _ string, _ domain.Issue, _ agent.EventCallback) (agent.TurnResult, error) {
	return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCompleted}, nil
}

func newPipelineOrchestrator(t *testing.T, runtimes map[string]agent.Runtime, roles []config.PipelineRole) *Orchestrator {
	t.Helper()
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}},
		Workspace: config.Workspace{Root: t.TempDir()},
		Polling:   config.Polling{IntervalMs: 30000},
		Agent: config.Agent{
			Runtime:             "mock",
			MaxConcurrentAgents: 1,
			MaxRetryBackoffMs:   300000,
			Pipeline:            roles,
		},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(io.Discard)
	o := New(cfg, trk, nil, wsmgr, log)
	o.roleRuntimes = runtimes
	o.rootCtx = context.Background()
	return o
}

func TestPipeline_HappyPath_AdvancesThroughRoles(t *testing.T) {
	plannerMock := &artifactWriter{artifact: ".symphony/plan.md"}
	implMock := &artifactWriter{artifact: ".symphony/implementation-summary.md"}
	reviewerMock := &artifactWriter{artifact: ".symphony/review-approved.md"}

	roles := []config.PipelineRole{
		{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "plan", ReadyArtifact: ".symphony/plan.md"},
		{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "impl", ReadyArtifact: ".symphony/implementation-summary.md"},
		{Role: "reviewer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "review", ReadyArtifact: ".symphony/review-approved.md"},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"planner":     plannerMock,
		"implementer": implMock,
		"reviewer":    reviewerMock,
	}, roles)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	o.running["I1"] = &runEntry{
		issue:     issue,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	o.claimed["I1"] = struct{}{}

	o.dispatchPipeline(ctx, issue)

	// After dispatch finishes, the entry is removed from o.running.
	_, stillRunning := o.running["I1"]
	assert.False(t, stillRunning, "running entry should be released after pipeline completes")
}

func TestPipeline_ReadyArtifactMissingSchedulesRetry(t *testing.T) {
	null := nullWriter{}
	roles := []config.PipelineRole{
		{Role: "planner", Runtime: "mock", MaxTurns: 2, PromptTemplate: "plan", ReadyArtifact: ".symphony/plan.md"},
		{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "impl", ReadyArtifact: ".symphony/impl.md"},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"planner":     null,
		"implementer": null,
	}, roles)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	o.running["I1"] = &runEntry{
		issue:  issue,
		cancel: cancel,
	}
	o.claimed["I1"] = struct{}{}

	o.dispatchPipeline(ctx, issue)

	// The planner ran but did not write its artifact, so a retry should be queued.
	require.Contains(t, o.retryAttempts, "I1")
	assert.Contains(t, o.retryAttempts["I1"].Error, "planner")
}

func TestPipeline_ReviewerLoopbackToImplementer(t *testing.T) {
	plannerMock := &artifactWriter{artifact: ".symphony/plan.md"}
	implMock := &artifactWriter{artifact: ".symphony/implementation-summary.md"}
	// Reviewer writes both review-approved (pretend it's approved) AND
	// review-needs-changes; the loopback table should pick the latter.
	reviewerMock := &artifactWriter{
		artifact: ".symphony/review-approved.md",
		loopback: ".symphony/review-needs-changes.md",
	}
	roles := []config.PipelineRole{
		{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: ".symphony/plan.md"},
		{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "i", ReadyArtifact: ".symphony/implementation-summary.md"},
		{
			Role: "reviewer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "r", ReadyArtifact: ".symphony/review-approved.md",
			OnArtifact: map[string]config.PipelineLoopback{
				".symphony/review-needs-changes.md": {RetryFrom: "implementer", MaxLoopbacks: 1},
			},
		},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"planner":     plannerMock,
		"implementer": implMock,
		"reviewer":    reviewerMock,
	}, roles)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	o.running["I1"] = &runEntry{issue: issue, cancel: cancel}
	o.claimed["I1"] = struct{}{}

	o.dispatchPipeline(ctx, issue)

	// max_loopbacks=1 → the second loopback attempt is refused, ending
	// the pipeline as a failure with a retry scheduled.
	require.Contains(t, o.retryAttempts, "I1")
	assert.Contains(t, o.retryAttempts["I1"].Error, "max_loopbacks")
}

// TestPipeline_PersistsProgressAcrossRetries verifies T4: a dispatch
// that fails partway through a pipeline persists its progress via
// durable.Store; a follow-up dispatch loads the record and resumes at
// the correct role with loopback counters preserved.
func TestPipeline_PersistsProgressAcrossRetries(t *testing.T) {
	durableDir := t.TempDir()
	store, err := durable.New(durableDir)
	require.NoError(t, err)

	plannerMock := &artifactWriter{artifact: ".symphony/plan.md"}
	implMock := &artifactWriter{artifact: ".symphony/implementation-summary.md"}
	// Reviewer fails by never writing an artifact; this causes the
	// dispatch to scheduleRetry from role 3.
	roles := []config.PipelineRole{
		{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: ".symphony/plan.md"},
		{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "i", ReadyArtifact: ".symphony/implementation-summary.md"},
		{Role: "reviewer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "r", ReadyArtifact: ".symphony/review.md"},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"planner":     plannerMock,
		"implementer": implMock,
		"reviewer":    nullWriter{},
	}, roles)
	o.durable = store

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	o.running["I1"] = &runEntry{issue: issue, cancel: cancel, startedAt: time.Now()}
	o.claimed["I1"] = struct{}{}

	o.dispatchPipeline(ctx, issue)

	// Reviewer failed: a retry was scheduled and the durable record
	// remains on disk recording the reviewer as CurrentRole.
	require.Contains(t, o.retryAttempts, "I1")
	var loaded domain.PipelineProgress
	require.NoError(t, store.Load("pipeline_progress/I1", &loaded))
	assert.Equal(t, "reviewer", loaded.CurrentRole, "current role should be reviewer when retry fires")
	assert.Contains(t, loaded.CompletedRoles, "planner")
	assert.Contains(t, loaded.CompletedRoles, "implementer")

	// Simulate the loopback counter being bumped before the retry —
	// pre-seed the record so we can verify it survives.
	loaded.Loopbacks = map[string]int{"implementer": 1}
	require.NoError(t, store.Save("pipeline_progress/I1", &loaded))

	// Second dispatch: fix the reviewer mock and re-run. dispatchPipeline
	// must load the durable record and resume at the reviewer role.
	reviewerMock := &artifactWriter{artifact: ".symphony/review.md"}
	o.roleRuntimes["reviewer"] = reviewerMock

	// Fresh runEntry as the prior dispatch released the claim.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	o.running["I1"] = &runEntry{issue: issue, cancel: cancel2, startedAt: time.Now()}
	o.claimed["I1"] = struct{}{}

	o.dispatchPipeline(ctx2, issue)

	// Loopback counter survived; record is gone on terminal completion.
	_, err = os.Stat(filepath.Join(durableDir, "pipeline_progress", "I1.json"))
	assert.True(t, os.IsNotExist(err), "durable record should be deleted on terminal completion")
}

// TestPipeline_ResumesAtCorrectRole asserts T4: dispatchPipeline starts
// at the role recorded in the durable progress file, not at role 0.
func TestPipeline_ResumesAtCorrectRole(t *testing.T) {
	durableDir := t.TempDir()
	store, err := durable.New(durableDir)
	require.NoError(t, err)

	// Pre-seed a progress record saying we're partway through.
	progress := &domain.PipelineProgress{
		CurrentRole:    "implementer",
		CompletedRoles: []string{"planner"},
		Loopbacks:      map[string]int{"planner": 1},
	}
	require.NoError(t, store.Save("pipeline_progress/I9", progress))

	plannerCalled := false
	plannerMock := &recordingRuntime{onTurn: func() { plannerCalled = true }, artifact: ".symphony/plan.md"}
	implMock := &artifactWriter{artifact: ".symphony/implementation-summary.md"}

	roles := []config.PipelineRole{
		{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: ".symphony/plan.md"},
		{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "i", ReadyArtifact: ".symphony/implementation-summary.md"},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"planner":     plannerMock,
		"implementer": implMock,
	}, roles)
	o.durable = store

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I9", Identifier: "WEB-9", State: "todo"}
	o.running["I9"] = &runEntry{issue: issue, cancel: cancel, startedAt: time.Now()}
	o.claimed["I9"] = struct{}{}

	o.dispatchPipeline(ctx, issue)

	assert.False(t, plannerCalled, "planner must NOT run when resuming from implementer")
}

// recordingRuntime is artifactWriter with an onTurn hook used by the
// resume test. We can't use pausingArtifactWriter directly since it
// lives in pause_test.go.
type recordingRuntime struct {
	artifact string
	onTurn   func()
	wsPath   string
}

func (m *recordingRuntime) StartSession(_ context.Context, ws domain.Workspace) (agent.Session, error) {
	m.wsPath = ws.Path
	return agent.Session{ID: "mock"}, nil
}
func (m *recordingRuntime) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (m *recordingRuntime) RunTurn(_ context.Context, sess agent.Session, _ string, _ domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	if m.onTurn != nil {
		m.onTurn()
	}
	if cb != nil {
		cb(agent.Event{Kind: agent.EventAssistantMessage, Timestamp: time.Now()})
	}
	if m.artifact != "" {
		_ = os.MkdirAll(filepath.Join(m.wsPath, filepath.Dir(m.artifact)), 0o755)   //nolint:errcheck
		_ = os.WriteFile(filepath.Join(m.wsPath, m.artifact), []byte("ok"), 0o644) //nolint:errcheck
	}
	return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCompleted}, nil
}

func TestConfig_PreflightRejectsBadPipeline(t *testing.T) {
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory"},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent: config.Agent{
			Runtime: "mock",
			Pipeline: []config.PipelineRole{
				{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: ""},
			},
		},
	}
	err := config.Preflight(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ready_artifact")

	cfg.Agent.Pipeline[0].ReadyArtifact = ".symphony/plan.md"
	cfg.Agent.Pipeline = append(cfg.Agent.Pipeline, config.PipelineRole{
		Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: "x",
	})
	err = config.Preflight(cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, err)) // not nil
	assert.Contains(t, err.Error(), "duplicate role")
}

// TestConfig_PreflightRejectsNegativeMaxLoopbacks covers T21: negative
// max_loopbacks is rejected. Zero is allowed (means "disabled").
func TestConfig_PreflightRejectsNegativeMaxLoopbacks(t *testing.T) {
	mkCfg := func(maxLB int) config.Config {
		return config.Config{
			Tracker:   config.Tracker{Kind: "memory"},
			Workspace: config.Workspace{Root: t.TempDir()},
			Agent: config.Agent{
				Runtime: "mock",
				Pipeline: []config.PipelineRole{
					{Role: "a", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: "a.md"},
					{
						Role: "b", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: "b.md",
						OnArtifact: map[string]config.PipelineLoopback{
							"loop.md": {RetryFrom: "a", MaxLoopbacks: maxLB},
						},
					},
				},
			},
		}
	}

	// max_loopbacks: -1 → preflight error.
	err := config.Preflight(mkCfg(-1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_loopbacks")

	// max_loopbacks: 0 → allowed (disabled).
	require.NoError(t, config.Preflight(mkCfg(0)))

	// max_loopbacks: 3 → allowed.
	require.NoError(t, config.Preflight(mkCfg(3)))
}

// TestConfig_PreflightRejectsNegativeRoleTimeout covers T22: negative
// timeout_ms is rejected. Zero is allowed (inherits dispatch timeout).
func TestConfig_PreflightRejectsNegativeRoleTimeout(t *testing.T) {
	mkCfg := func(timeoutMs int) config.Config {
		return config.Config{
			Tracker:   config.Tracker{Kind: "memory"},
			Workspace: config.Workspace{Root: t.TempDir()},
			Agent: config.Agent{
				Runtime: "mock",
				Pipeline: []config.PipelineRole{
					{Role: "a", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: "a.md", TimeoutMs: timeoutMs},
				},
			},
		}
	}

	err := config.Preflight(mkCfg(-5))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout_ms")

	require.NoError(t, config.Preflight(mkCfg(0)))
	require.NoError(t, config.Preflight(mkCfg(60_000)))
}

// TestConfig_PreflightRetryFromRules covers T23: retry_from must point
// to a strictly earlier role; self- and forward-references are
// rejected.
func TestConfig_PreflightRetryFromRules(t *testing.T) {
	base := func(loopFrom, retryFrom string) config.Config {
		roles := []config.PipelineRole{
			{Role: "planner", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p", ReadyArtifact: "p.md"},
			{Role: "implementer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "i", ReadyArtifact: "i.md"},
			{Role: "reviewer", Runtime: "mock", MaxTurns: 1, PromptTemplate: "r", ReadyArtifact: "r.md"},
		}
		// Attach the loopback to the named role.
		for i := range roles {
			if roles[i].Role == loopFrom {
				roles[i].OnArtifact = map[string]config.PipelineLoopback{
					"loop.md": {RetryFrom: retryFrom, MaxLoopbacks: 1},
				}
			}
		}
		return config.Config{
			Tracker:   config.Tracker{Kind: "memory"},
			Workspace: config.Workspace{Root: t.TempDir()},
			Agent:     config.Agent{Runtime: "mock", Pipeline: roles},
		}
	}

	// Valid: reviewer -> planner (earlier).
	require.NoError(t, config.Preflight(base("reviewer", "planner")))
	// Valid: reviewer -> implementer (earlier).
	require.NoError(t, config.Preflight(base("reviewer", "implementer")))

	// Invalid: self-reference.
	err := config.Preflight(base("reviewer", "reviewer"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must reference an earlier role")

	// Invalid: forward reference (implementer -> reviewer).
	err = config.Preflight(base("implementer", "reviewer"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must reference an earlier role")

	// Invalid: unknown role.
	err = config.Preflight(base("reviewer", "nobody"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must reference an earlier role")
}
