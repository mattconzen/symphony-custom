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
