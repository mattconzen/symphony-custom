package orchestrator_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

// eventually polls fn with ~10ms intervals until it returns true or deadline is exceeded.
func eventually(t *testing.T, deadline time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("eventually: condition not met within deadline")
}

func buildCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Polling.IntervalMs = 50
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	cfg.Hooks.TimeoutMs = 5_000
	return cfg
}

// recorderIface is the extra interface exposed by mock runtime.
type recorderIface interface {
	RecordedPrompts() []mock.RecordedTurn
	RecordedTurnCount() int
}

func TestOrchestrator_HappyPath(t *testing.T) {
	cfg := buildCfg(t)

	issue := domain.Issue{
		ID:         "issue-1",
		Identifier: "TEST-1",
		Title:      "Implement feature",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})

	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{TotalTokens: 42}},
		},
	})

	ws := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)

	orch := orchestrator.New(cfg, tr, rt, ws, log).
		WithPromptTemplate("You are working on issue {{ issue.identifier }}.")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- orch.Run(ctx)
	}()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	// Assert: mock received at least one turn within deadline.
	eventually(t, 3*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})

	// The prompt should contain the issue identifier.
	prompts := rec.RecordedPrompts()
	require.NotEmpty(t, prompts)
	assert.Contains(t, prompts[0].Prompt, "TEST-1")

	// Cancel and verify clean shutdown.
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down within 2s")
	}
}

func TestOrchestrator_WorkspaceCreated(t *testing.T) {
	cfg := buildCfg(t)

	issue := domain.Issue{
		ID:         "issue-ws",
		Identifier: "WS-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)
	eventually(t, 3*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})

	// Workspace directory should exist under wsRoot.
	wsPath := cfg.Workspace.Root + "/" + issue.WorkspaceKey()
	_, err := os.Stat(wsPath)
	assert.NoError(t, err, "workspace dir should exist after dispatch")

	cancel()
}

func TestOrchestrator_RequestRefresh(t *testing.T) {
	cfg := buildCfg(t)
	tr := memory.New(nil)
	rt := mock.New(mock.MockOpts{Turns: []mock.TurnScript{{Status: agent.TurnCompleted}}})
	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	// Without Run, refreshC is never drained: first send queues, second coalesces.
	assert.True(t, orch.RequestRefresh(), "first call must enqueue (returns true)")
	assert.False(t, orch.RequestRefresh(), "second call must coalesce (returns false)")

	// Run the orchestrator with a long poll interval so we can attribute the
	// next dispatch unambiguously to a refresh trigger rather than a tick.
	cfg.Polling.IntervalMs = 60_000
	issue := domain.Issue{ID: "i-refresh", Identifier: "RF-1", State: "In Progress"}
	tr2 := memory.New([]domain.Issue{issue})
	orch2 := orchestrator.New(cfg, tr2, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() { _ = orch2.Run(ctx); close(done) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	// Wait for the initial immediate tick to drain the candidate, then trigger
	// a refresh and observe the second dispatch attempt. The mock memory
	// tracker keeps yielding the same issue, but reconcile + dispatch run on
	// every tick — so we just need turn count to grow past the initial.
	eventually(t, 3*time.Second, func() bool { return rec.RecordedTurnCount() >= 1 })
	beforeTriggerCount := rec.RecordedTurnCount()

	// After the initial tick is over, refreshC should be drainable again.
	assert.True(t, orch2.RequestRefresh(), "third call (after drain) must enqueue again")

	// Tick interval is 60s; if we see another turn, it can only be from refresh.
	eventually(t, 3*time.Second, func() bool {
		return rec.RecordedTurnCount() > beforeTriggerCount
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down in 2s")
	}
}

func TestOrchestrator_MultipleIssues(t *testing.T) {
	cfg := buildCfg(t)

	issues := []domain.Issue{
		{ID: "i1", Identifier: "A-1", State: "In Progress"},
		{ID: "i2", Identifier: "A-2", State: "In Progress"},
	}
	tr := memory.New(issues)
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	// Expect at least 2 turn invocations (one per issue).
	eventually(t, 3*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 2
	})

	cancel()
}
