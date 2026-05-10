package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// TestPause_BetweenPipelineRoles verifies T3: pause requested after role
// 1 completes must block role 2 until Resume; the run state must be
// observable as "paused" while waiting.
func TestPause_BetweenPipelineRoles(t *testing.T) {
	role1Done := make(chan struct{})
	role2Started := make(chan struct{})

	role1 := &pausingArtifactWriter{
		artifact: ".symphony/role1.md",
		onTurn:   func() { close(role1Done) },
	}
	role2 := &pausingArtifactWriter{
		artifact: ".symphony/role2.md",
		onTurn:   func() { close(role2Started) },
	}
	role3 := &artifactWriter{artifact: ".symphony/role3.md"}

	roles := []config.PipelineRole{
		{Role: "r1", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p1", ReadyArtifact: ".symphony/role1.md"},
		{Role: "r2", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p2", ReadyArtifact: ".symphony/role2.md"},
		{Role: "r3", Runtime: "mock", MaxTurns: 1, PromptTemplate: "p3", ReadyArtifact: ".symphony/role3.md"},
	}
	o := newPipelineOrchestrator(t, map[string]agent.Runtime{
		"r1": role1,
		"r2": role2,
		"r3": role3,
	}, roles)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.rootCtx = ctx

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	o.running["I1"] = &runEntry{issue: issue, cancel: cancel, startedAt: time.Now()}
	o.claimed["I1"] = struct{}{}

	// Request pause before dispatch so it fires after role 1 finishes the
	// first observePauseOrCancel between-role gate.
	require.NoError(t, o.RequestPause("WEB-1"))

	done := make(chan struct{})
	go func() {
		o.dispatchPipeline(ctx, issue)
		close(done)
	}()

	// Wait for role 1 to complete its turn.
	select {
	case <-role1Done:
	case <-time.After(2 * time.Second):
		t.Fatal("role1 never ran")
	}

	// Poll for paused state (set by observePauseOrCancel between roles).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := o.RunStateOf("WEB-1")
		if state == "paused" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := o.RunStateOf("WEB-1")
	require.Equal(t, "paused", state, "dispatch should be paused between roles")

	// role2 must not have started yet.
	select {
	case <-role2Started:
		t.Fatal("role2 should not have started while paused")
	case <-time.After(50 * time.Millisecond):
	}

	// Resume; role2 should now run.
	require.NoError(t, o.Resume("WEB-1"))
	select {
	case <-role2Started:
	case <-time.After(2 * time.Second):
		t.Fatal("role2 never ran after resume")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchPipeline did not return after resume")
	}
}

// pausingArtifactWriter is artifactWriter with an onTurn callback fired
// whenever RunTurn is invoked. Used to coordinate test orchestration.
type pausingArtifactWriter struct {
	artifact string
	onTurn   func()
	wsPath   string
}

func (m *pausingArtifactWriter) StartSession(_ context.Context, ws domain.Workspace) (agent.Session, error) {
	m.wsPath = ws.Path
	return agent.Session{ID: "mock"}, nil
}
func (m *pausingArtifactWriter) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (m *pausingArtifactWriter) RunTurn(_ context.Context, sess agent.Session, _ string, _ domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	if m.onTurn != nil {
		m.onTurn()
	}
	if cb != nil {
		cb(agent.Event{Kind: agent.EventAssistantMessage, Timestamp: time.Now()})
	}
	if m.artifact != "" {
		_ = os.MkdirAll(filepath.Join(m.wsPath, filepath.Dir(m.artifact)), 0o755) //nolint:errcheck
		_ = os.WriteFile(filepath.Join(m.wsPath, m.artifact), []byte("ok"), 0o644) //nolint:errcheck
	}
	return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCompleted}, nil
}

// TestPause_DoubleResumeDoesNotPanic covers the safeClose-replacement
// guard (T18). Two back-to-back Resume calls — and an interleaved
// RequestCancel — must not panic on double close(pauseCh).
func TestPause_DoubleResumeDoesNotPanic(t *testing.T) {
	o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")

	require.NoError(t, o.RequestPause("WEB-1"))
	require.NoError(t, o.Resume("WEB-1"))
	// Second Resume: not paused anymore -> ErrNotPaused, no panic.
	err := o.Resume("WEB-1")
	assert.True(t, errors.Is(err, ErrNotPaused))

	// Pause again, then cancel (which also closes pauseCh).
	require.NoError(t, o.RequestPause("WEB-1"))
	require.NoError(t, o.RequestCancel("WEB-1"))
	// Another cancel is a no-op (entry still present, channel already
	// nil-ed out under the lock — close path is guarded).
	require.NoError(t, o.RequestCancel("WEB-1"))
}

// TestPause_ConcurrentResumeAndCancel exercises the close-once guard
// under simulated racing closers (T18). With the prior recover()-based
// safeClose this could mask real double-close bugs; the new pauseClosed
// guard provably closes exactly once.
func TestPause_ConcurrentResumeAndCancel(t *testing.T) {
	for i := 0; i < 50; i++ {
		o := buildTestOrchestratorWithRunning(t, "I1", "WEB-1")
		require.NoError(t, o.RequestPause("WEB-1"))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = o.Resume("WEB-1") //nolint:errcheck
		}()
		go func() {
			defer wg.Done()
			_ = o.RequestCancel("WEB-1") //nolint:errcheck
		}()
		wg.Wait()
		// No panic = pass. State should be either running or cancel_requested.
		state, _ := o.RunStateOf("WEB-1")
		assert.Contains(t, []string{"running", "cancel_requested"}, state)
	}
}

// ensure agent package stays used in the file (mock factory side-effect)
var _ = agent.EventCallback(nil)
