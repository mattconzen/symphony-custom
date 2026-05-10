package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

func buildTestOrchestratorWithRunning(t *testing.T, issueID, identifier string) *Orchestrator {
	t.Helper()
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(nil)
	rt := mock.New(mock.MockOpts{})
	o := New(cfg, trk, rt, wsmgr, log)
	o.rootCtx = context.Background()

	ctx, cancel := context.WithCancel(context.Background())
	o.running[issueID] = &runEntry{
		issue:     domain.Issue{ID: issueID, Identifier: identifier, State: "todo"},
		cancel:    cancel,
		startedAt: time.Now(),
	}
	o.claimed[issueID] = struct{}{}
	t.Cleanup(func() { cancel() })
	_ = ctx
	return o
}

func TestPause_RequestThenResume(t *testing.T) {
	o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")

	require.NoError(t, o.RequestPause("WEB-1"))
	state, ok := o.RunStateOf("WEB-1")
	require.True(t, ok)
	assert.Equal(t, "pause_requested", state)

	// Promote to paused as if the turn loop reached the gate.
	o.mu.Lock()
	o.running["I1"].state = runStatePaused
	o.mu.Unlock()

	require.NoError(t, o.Resume("WEB-1"))
	state, _ = o.RunStateOf("WEB-1")
	assert.Equal(t, "running", state)
}

func TestPause_DoublePauseIsIdempotent(t *testing.T) {
	o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")

	require.NoError(t, o.RequestPause("WEB-1"))
	require.NoError(t, o.RequestPause("WEB-1"))
	state, _ := o.RunStateOf("WEB-1")
	assert.Equal(t, "pause_requested", state)
}

func TestResume_WithoutPauseReturnsErrNotPaused(t *testing.T) {
	o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")
	err := o.Resume("WEB-1")
	assert.True(t, errors.Is(err, ErrNotPaused))
}

func TestPause_NotRunning(t *testing.T) {
	o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")
	err := o.RequestPause("UNKNOWN")
	assert.True(t, errors.Is(err, ErrNotRunning))
}

func TestCancel_CancelsContextAndSuppressesRetry(t *testing.T) {
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(nil)
	rt := mock.New(mock.MockOpts{})
	o := New(cfg, trk, rt, wsmgr, log)
	o.rootCtx = context.Background()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.running["I1"] = &runEntry{
		issue:  domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"},
		cancel: cancel,
	}
	o.claimed["I1"] = struct{}{}

	require.NoError(t, o.RequestCancel("WEB-1"))
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctx should have been cancelled by RequestCancel")
	}
	assert.True(t, o.cancelRequested("I1"))
}

func TestRunEntryToSnapshotIncludesRunState(t *testing.T) {
	e := &runEntry{
		issue: domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"},
		state: runStatePaused,
	}
	snap := runEntryToSnapshot(e)
	assert.Equal(t, "paused", snap.RunState)
}

// ensure agent package stays used in the file (mock factory side-effect)
var _ = agent.EventCallback(nil)
