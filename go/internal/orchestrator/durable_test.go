package orchestrator

import (
	"context"
	"testing"

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
