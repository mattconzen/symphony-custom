package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
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

// fixedNow is the synthetic "now" baked into the golden snapshot fixture.
var fixedNow = time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)

// seedRunning installs a synthetic running entry on o for snapshot testing.
// Mirrors the single-issue projection produced by Elixir's StatusDashboard.
func seedRunning(o *Orchestrator, e *runEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running[e.issue.ID] = e
	o.claimed[e.issue.ID] = struct{}{}
}

// seedRetry installs a synthetic retry entry on o for snapshot testing.
func seedRetry(o *Orchestrator, r *domain.RetryEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retryAttempts[r.IssueID] = r
}

func TestBuildSnapshot_GoldenJSON(t *testing.T) {
	cfg := config.Config{}
	o := New(cfg, nil, nil, nil, observability.New(&bytes.Buffer{}))

	o.codexTotals = observability.TokenTotals{
		TotalTokens:    1500,
		InputTokens:    900,
		OutputTokens:   600,
		SecondsRunning: 42,
	}
	o.rateLimits = map[string]any{
		"primary": map[string]any{
			"used_percent": 12.5,
			"resets_at":    "2026-05-09T13:00:00Z",
		},
		"secondary": nil,
	}

	seedRunning(o, &runEntry{
		issue: domain.Issue{
			ID:         "issue-1",
			Identifier: "TEST-1",
			State:      "In Progress",
		},
		startedAt:     time.Date(2026, 5, 9, 11, 55, 0, 0, time.UTC),
		sessionID:     "sess-1",
		workspacePath: "/tmp/work/TEST-1",
		workerHost:    "worker-a",
		turnCount:     2,
		lastEvent:     "assistant_message",
		lastMessage:   "Refactoring the parser.",
		lastEventAt:   time.Date(2026, 5, 9, 11, 59, 30, 0, time.UTC),
		tokens: observability.EntryTokens{
			TotalTokens:  800,
			InputTokens:  500,
			OutputTokens: 300,
		},
	})
	// TEST-2 has no worker_host and no last_message → both encode JSON null.
	seedRunning(o, &runEntry{
		issue: domain.Issue{
			ID:         "issue-2",
			Identifier: "TEST-2",
			State:      "In Progress",
		},
		startedAt:     time.Date(2026, 5, 9, 11, 58, 0, 0, time.UTC),
		sessionID:     "sess-2",
		workspacePath: "/tmp/work/TEST-2",
		turnCount:     1,
		lastEvent:     "tool_call",
		lastEventAt:   time.Date(2026, 5, 9, 11, 59, 45, 0, time.UTC),
		tokens: observability.EntryTokens{
			TotalTokens:  700,
			InputTokens:  400,
			OutputTokens: 300,
		},
	})

	dueAt := time.Date(2026, 5, 9, 12, 5, 0, 0, time.UTC)
	seedRetry(o, &domain.RetryEntry{
		IssueID:    "issue-3",
		Identifier: "TEST-3",
		Attempt:    2,
		DueAtMs:    dueAt.UnixMilli(),
		Error:      "transient agent failure",
	})

	snap := BuildSnapshot(o)
	// Force GeneratedAt to the fixture's value so the marshaled bytes are
	// stable; we don't need to assert wall-clock here.
	snap.GeneratedAt = fixedNow
	// Force Polling to the fixture's values (BuildSnapshot derives these from
	// config + lastPollAt; this golden test isn't covering that derivation).
	snap.Polling = observability.Polling{
		Checking:       false,
		PollIntervalMs: 2000,
		NextPollInMs:   1500,
	}

	got, err := json.MarshalIndent(snap, "", "  ")
	require.NoError(t, err)

	goldenPath := filepath.Join("..", "observability", "testdata", "snapshot.golden.json")
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err)

	gotTrimmed := bytes.TrimRight(got, "\n")
	wantTrimmed := bytes.TrimRight(want, "\n")

	if !bytes.Equal(gotTrimmed, wantTrimmed) {
		t.Fatalf("BuildSnapshot JSON mismatch.\n--- got ---\n%s\n--- want ---\n%s", gotTrimmed, wantTrimmed)
	}
}

func TestBuildSnapshot_EmptyOrchestrator(t *testing.T) {
	o := New(config.Config{}, nil, nil, nil, observability.New(&bytes.Buffer{}))
	snap := o.Snapshot()

	assert.Equal(t, 0, snap.Counts.Running)
	assert.Equal(t, 0, snap.Counts.Retrying)
	assert.NotNil(t, snap.Running, "running slice must marshal as [], not null")
	assert.NotNil(t, snap.Retrying, "retrying slice must marshal as [], not null")
	assert.False(t, snap.GeneratedAt.IsZero())
}

func TestWithUpdateCallback_FiresOnDispatch(t *testing.T) {
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	cfg.Polling.IntervalMs = 50
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 2
	cfg.Agent.MaxRetryBackoffMs = 60_000
	cfg.Hooks.TimeoutMs = 5_000

	issue := domain.Issue{
		ID:         "issue-cb-1",
		Identifier: "CB-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{TotalTokens: 11, InputTokens: 7, OutputTokens: 4}},
		},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})

	var fired atomic.Int32
	orch := New(cfg, tr, rt, wsMgr, log).
		WithUpdateCallback(func() { fired.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		_ = orch.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fired.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down in 2s")
	}

	// Expect at least: dispatch start + turn complete. In practice we get
	// more (release-claim, reconcile), but two is the floor that proves the
	// callback wired into the dispatch path rather than firing once at
	// startup by accident.
	assert.GreaterOrEqual(t, int(fired.Load()), 2,
		"OnUpdate must fire at minimum once per dispatch start and once per turn-complete")
}
