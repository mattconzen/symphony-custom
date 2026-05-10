package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/durable"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

func TestOrchestrator_DurableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := durable.New(dir)
	require.NoError(t, err)

	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory", ActiveStates: []string{"todo"}},
		Workspace: config.Workspace{Root: t.TempDir()},
		Polling:   config.Polling{IntervalMs: 30000},
		Agent:     config.Agent{Runtime: "mock", MaxConcurrentAgents: 1, MaxRetryBackoffMs: 300000},
	}

	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(nil)
	rt := mock.New(mock.MockOpts{})

	first := New(cfg, trk, rt, wsmgr, log).WithDurableStore(store)
	// Seed in-memory state that should survive a restart.
	first.mu.Lock()
	first.retryAttempts["I1"] = &domain.RetryEntry{IssueID: "I1", Identifier: "I-1", Attempt: 3, Error: "boom", DueAtMs: 12345}
	first.pullRequests["I-1"] = domain.PullRequest{URL: "https://github.com/x/y/pull/7", Number: 7, State: "open", Source: "agent_event"}
	first.agentTotals.InputTokens = 100
	first.agentTotals.OutputTokens = 50
	first.mu.Unlock()
	first.saveDurable()

	// Construct a fresh orchestrator with the same store and confirm state restored.
	second := New(cfg, trk, rt, wsmgr, log).WithDurableStore(store)
	require.NoError(t, second.loadDurable())

	second.mu.Lock()
	defer second.mu.Unlock()
	require.Contains(t, second.retryAttempts, "I1")
	assert.Equal(t, 3, second.retryAttempts["I1"].Attempt)
	require.Contains(t, second.pullRequests, "I-1")
	assert.Equal(t, "open", second.pullRequests["I-1"].State)
	assert.Equal(t, 100, second.agentTotals.InputTokens)
	assert.Equal(t, 50, second.agentTotals.OutputTokens)
}

// TestOrchestrator_DurableSaveErrLogRateLimited simulates 10 consecutive
// save failures with the same error string and asserts the warn log fires
// at most twice (initial + maybe a rotation under 30s would be zero, so
// strictly: exactly one warn for the same error inside the window).
func TestOrchestrator_DurableSaveErrLogRateLimited(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory"},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	rt := mock.New(mock.MockOpts{})
	o := New(cfg, trk, rt, wsmgr, log)

	ps := &durablePersisterState{}
	sameErr := errors.New("disk: i/o error")
	for i := 0; i < 10; i++ {
		o.maybeLogSaveErr(ps, sameErr)
	}

	warns := countWarnsContaining(buf.String(), "save failed")
	assert.LessOrEqual(t, warns, 2, "expected ≤2 warns over 10 same-error ticks, got %d", warns)
	assert.GreaterOrEqual(t, warns, 1, "expected ≥1 warn so operators see the failure")
}

// TestOrchestrator_DurableSaveErrLogsNewErrorImmediately confirms a fresh
// error string bypasses the rate limit so operators see new failure modes
// even when an older one is still suppressed.
func TestOrchestrator_DurableSaveErrLogsNewErrorImmediately(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory"},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	o := New(cfg, memory.New(nil), mock.New(mock.MockOpts{}), workspace.NewManager(cfg), log)

	ps := &durablePersisterState{}
	o.maybeLogSaveErr(ps, errors.New("disk: i/o error"))
	o.maybeLogSaveErr(ps, errors.New("disk: i/o error"))      // rate-limited
	o.maybeLogSaveErr(ps, errors.New("disk: read-only"))      // new error → log
	o.maybeLogSaveErr(ps, errors.New("disk: read-only"))      // rate-limited

	warns := countWarnsContaining(buf.String(), "save failed")
	assert.Equal(t, 2, warns)
}

// TestOrchestrator_DurableSaveLogsRecoveryOnce verifies the "saves
// recovered" Info log fires exactly once after a successful save that
// follows one or more failures.
func TestOrchestrator_DurableSaveLogsRecoveryOnce(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)

	dir := t.TempDir()
	store, err := durable.New(dir)
	require.NoError(t, err)
	defer store.Close()

	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory"},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	o := New(cfg, memory.New(nil), mock.New(mock.MockOpts{}), workspace.NewManager(cfg), log).
		WithDurableStore(store)

	ps := &durablePersisterState{}
	// Simulate prior failure so hadError is set.
	ps.hadError = true
	ps.lastErrStr = "previous"
	ps.lastErrLogAt = time.Now()

	// A real successful save against the real store.
	o.saveDurableWithState(ps)

	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "durable: orchestrator saves recovered"))
	assert.False(t, ps.hadError, "hadError flag should reset after recovery")
}

func countWarnsContaining(out, substr string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, substr) {
			count++
		}
	}
	return count
}

func TestOrchestrator_DurableNilStoreIsNoOp(t *testing.T) {
	cfg := config.Config{
		Tracker:   config.Tracker{Kind: "memory"},
		Workspace: config.Workspace{Root: t.TempDir()},
		Agent:     config.Agent{Runtime: "mock"},
	}
	trk := memory.New(nil)
	wsmgr := workspace.NewManager(cfg)
	log := observability.New(nil)
	rt := mock.New(mock.MockOpts{})

	o := New(cfg, trk, rt, wsmgr, log)
	require.NoError(t, o.loadDurable())
	o.saveDurable() // must not panic with nil store

	// Verify the persister goroutine returns quickly when ctx is done and store nil.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.runDurablePersister(ctx) // immediate return
}
