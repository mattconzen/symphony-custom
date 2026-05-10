package orchestrator

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
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

// cancellingRuntime returns TurnCancelled after observing a cancel
// trigger. It records workspace path so the test can verify post-run
// side-effects (e.g. that after_run did NOT fire).
type cancellingRuntime struct {
	turns        int32
	cancelOnTurn int32
	o            *Orchestrator
	identifier   string
}

func (c *cancellingRuntime) StartSession(_ context.Context, _ domain.Workspace) (agent.Session, error) {
	return agent.Session{ID: "mock"}, nil
}
func (c *cancellingRuntime) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (c *cancellingRuntime) RunTurn(ctx context.Context, sess agent.Session, _ string, _ domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	n := atomic.AddInt32(&c.turns, 1)
	if cb != nil {
		cb(agent.Event{Kind: agent.EventAssistantMessage, Timestamp: time.Now()})
	}
	if n == c.cancelOnTurn {
		// Simulate operator cancel: this issues both ctx cancellation
		// AND records runState=cancel_requested.
		_ = c.o.RequestCancel(c.identifier) //nolint:errcheck
		<-ctx.Done()
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCancelled}, ctx.Err()
	}
	return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnCompleted}, nil
}

// TestDispatch_SkipsAfterRunOnCancel asserts T19: when the run is
// cancelled mid-turn, the after_run hook is NOT invoked. We detect
// this by having after_run write a sentinel file; the test asserts
// the file does not exist after dispatch returns.
func TestDispatch_SkipsAfterRunOnCancel(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "after_run_fired")
	// `printf` is more portable than `touch` for path with spaces.
	hookCmd := "printf done > " + sentinel

	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}},
		Workspace: config.Workspace{Root: tmp},
		Polling:   config.Polling{IntervalMs: 30000},
		Agent: config.Agent{
			Runtime:             "mock",
			MaxTurns:            5,
			MaxConcurrentAgents: 1,
			MaxRetryBackoffMs:   300000,
		},
		Hooks: config.Hooks{AfterRun: hookCmd, TimeoutMs: 2000},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(io.Discard)

	rt := &cancellingRuntime{cancelOnTurn: 1}
	o := New(cfg, trk, rt, wsmgr, log)
	o.rootCtx = context.Background()
	rt.o = o
	rt.identifier = "WEB-1"

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.running["I1"] = &runEntry{
		issue:     issue,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	o.claimed["I1"] = struct{}{}

	o.dispatchOne(ctx, issue)

	// Sentinel must NOT exist because cancellation skipped after_run.
	_, err := os.Stat(sentinel)
	require.True(t, os.IsNotExist(err), "after_run hook should NOT have run on a cancelled dispatch")
}

// TestCancel_CleansUpDotSymphony asserts T20: after RequestCancel,
// the workspace's .symphony/ directory (including plan.md and any
// pipeline-progress files) is removed before the next dispatch begins.
func TestCancel_CleansUpDotSymphony(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}},
		Workspace: config.Workspace{Root: tmp},
		Agent:     config.Agent{Runtime: "mock"},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(io.Discard)
	o := New(cfg, trk, nil, wsmgr, log)
	o.rootCtx = context.Background()

	issue := domain.Issue{ID: "I1", Identifier: "WEB-1", State: "todo"}
	wsPath := filepath.Join(tmp, issue.WorkspaceKey())
	require.NoError(t, os.MkdirAll(filepath.Join(wsPath, ".symphony"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, ".symphony", "plan.md"), []byte("stale"), 0o644))

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.running["I1"] = &runEntry{
		issue:         issue,
		cancel:        cancel,
		workspacePath: wsPath,
		startedAt:     time.Now(),
	}
	o.claimed["I1"] = struct{}{}

	require.NoError(t, o.RequestCancel("WEB-1"))

	// .symphony/ must be gone after RequestCancel.
	_, err := os.Stat(filepath.Join(wsPath, ".symphony"))
	assert.True(t, os.IsNotExist(err), ".symphony should be removed on cancel")

	// Workspace dir itself MUST still exist (git tree preserved).
	info, err := os.Stat(wsPath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestDispatch_RunsAfterRunOnSuccess is the positive control: a normal
// (non-cancelled) dispatch DOES fire after_run.
func TestDispatch_RunsAfterRunOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "after_run_fired_ok")
	hookCmd := "printf done > " + sentinel

	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}, TerminalStates: []string{"done"}},
		Workspace: config.Workspace{Root: tmp},
		Polling:   config.Polling{IntervalMs: 30000},
		Agent: config.Agent{
			Runtime:             "mock",
			MaxTurns:            1,
			MaxConcurrentAgents: 1,
			MaxRetryBackoffMs:   300000,
		},
		Hooks: config.Hooks{AfterRun: hookCmd, TimeoutMs: 2000},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(io.Discard)

	rt := &cancellingRuntime{cancelOnTurn: 99} // never cancel
	o := New(cfg, trk, rt, wsmgr, log)
	o.rootCtx = context.Background()
	rt.o = o
	rt.identifier = "WEB-2"

	issue := domain.Issue{ID: "I2", Identifier: "WEB-2", State: "todo"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.running["I2"] = &runEntry{
		issue:     issue,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	o.claimed["I2"] = struct{}{}

	o.dispatchOne(ctx, issue)

	_, err := os.Stat(sentinel)
	assert.NoError(t, err, "after_run hook should fire on a successful dispatch")
}
