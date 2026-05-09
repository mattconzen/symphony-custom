package orchestrator_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// buildBetweenTurnsCfg builds a base config suitable for between_turns tests.
// wsRoot is set from t.TempDir().
func buildBetweenTurnsCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Polling.IntervalMs = 20
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	cfg.Hooks.TimeoutMs = 5_000
	return cfg
}

// activeIssue returns a simple in-progress issue.
func activeIssue(id, identifier string) domain.Issue {
	return domain.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      "Test issue",
		State:      "In Progress",
	}
}

// threeCompletedTurns returns a script for 3 TurnCompleted turns (issue stays active).
func threeCompletedTurns() []mock.TurnScript {
	return []mock.TurnScript{
		{Status: agent.TurnCompleted},
		{Status: agent.TurnCompleted},
		{Status: agent.TurnCompleted},
	}
}

// waitForTurnCount waits up to deadline for the mock to record at least n turns.
func waitForTurnCount(t *testing.T, rec recorderIface, n int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if rec.RecordedTurnCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d turns within %s, got %d", n, deadline, rec.RecordedTurnCount())
}

// TestBetweenTurns_HookFiresPerTurn verifies the hook runs between turns (not after the final turn).
// With 3 TurnCompleted turns, the hook fires twice: between turns 1→2 and 2→3.
func TestBetweenTurns_HookFiresPerTurn(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	wsRoot := cfg.Workspace.Root

	issue := activeIssue("bt-1", "BT-1")

	// The hook appends a line to a counter file inside the workspace.
	// We use $WS as a shorthand for the issue workspace path.
	issueWsKey := issue.WorkspaceKey()
	counterPath := filepath.Join(wsRoot, issueWsKey, "counter")
	cfg.Hooks.BetweenTurns = fmt.Sprintf("echo OK >> %q", counterPath)

	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         threeCompletedTurns(),
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	// Wait for all 3 turns to complete (max turns = 3).
	waitForTurnCount(t, rec, 3, 5*time.Second)
	cancel()

	// Allow a brief moment for the goroutine to complete its final cleanup.
	time.Sleep(50 * time.Millisecond)

	// Counter file should have exactly 2 lines (hook fired between turns 1→2 and 2→3).
	data, err := os.ReadFile(counterPath)
	require.NoError(t, err, "counter file should exist")
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	assert.Len(t, lines, 2, "hook should fire 2 times (between turns 1→2 and 2→3)")
}

// TestBetweenTurns_StdoutBecomesFeedback verifies that a non-zero hook exit
// causes its stdout to appear in the next turn's prompt under "Validation feedback".
func TestBetweenTurns_StdoutBecomesFeedback(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	cfg.Hooks.BetweenTurns = "printf 'lint:foo:1:2: bar\\n'; exit 1"

	issue := activeIssue("bt-2", "BT-2")
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         threeCompletedTurns(),
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	waitForTurnCount(t, rec, 3, 5*time.Second)
	cancel()

	prompts := rec.RecordedPrompts()
	require.GreaterOrEqual(t, len(prompts), 2, "expected at least 2 recorded turns")

	// Turn 2's prompt (index 1) must contain the lint output and the section heading.
	turn2Prompt := prompts[1].Prompt
	assert.Contains(t, turn2Prompt, "lint:foo:1:2: bar", "lint output should appear in turn 2 prompt")
	assert.Contains(t, turn2Prompt, "Validation feedback", "heading should appear in turn 2 prompt")

	// The run must NOT abort — we get all 3 turns.
	assert.GreaterOrEqual(t, rec.RecordedTurnCount(), 3, "run should complete all 3 turns")
}

// TestBetweenTurns_Timeout verifies that a timed-out hook surfaces a timeout
// message in the next turn's feedback section, and the run still proceeds.
func TestBetweenTurns_Timeout(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	cfg.Hooks.BetweenTurns = "sleep 5"
	cfg.Hooks.TimeoutMs = 200

	issue := activeIssue("bt-3", "BT-3")
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         threeCompletedTurns(),
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	// Hook takes ~200ms each time, 2 firings between turns → allow 10s total.
	waitForTurnCount(t, rec, 2, 10*time.Second)
	cancel()

	prompts := rec.RecordedPrompts()
	require.GreaterOrEqual(t, len(prompts), 2, "expected at least 2 recorded turns")

	// Turn 2's prompt must mention timed out.
	turn2Prompt := prompts[1].Prompt
	assert.Contains(t, turn2Prompt, "timed out", "timeout message should appear in turn 2 prompt")

	// The run still progresses past turn 1.
	assert.GreaterOrEqual(t, rec.RecordedTurnCount(), 2, "run should progress to turn 2")
}

// TestBetweenTurns_HookUnset verifies that with no between_turns hook configured,
// continuation prompts do NOT include any "Validation" section.
func TestBetweenTurns_HookUnset(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	// BetweenTurns is deliberately empty (regression guard).

	issue := activeIssue("bt-4", "BT-4")
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         threeCompletedTurns(),
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	waitForTurnCount(t, rec, 3, 5*time.Second)
	cancel()

	prompts := rec.RecordedPrompts()
	require.GreaterOrEqual(t, len(prompts), 2, "expected at least 2 recorded turns")

	// Turn 2's prompt (index 1) must NOT contain any validation section.
	turn2Prompt := prompts[1].Prompt
	assert.NotContains(t, turn2Prompt, "Validation feedback", "no validation section when hook is unset")
	assert.NotContains(t, turn2Prompt, "Validation", "no validation text when hook is unset")
}

// TestBetweenTurns_HookCwd verifies the hook runs with cwd set to the issue workspace.
func TestBetweenTurns_HookCwd(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	cfg.Hooks.BetweenTurns = "pwd > marker_file"

	issue := activeIssue("bt-5", "BT-5")
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		// Only 2 turns so the hook fires once (between turns 1 and 2).
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
		},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	waitForTurnCount(t, rec, 2, 5*time.Second)
	cancel()
	time.Sleep(50 * time.Millisecond)

	// The marker file must exist inside the issue's workspace directory.
	issueWsPath := filepath.Join(cfg.Workspace.Root, issue.WorkspaceKey())
	markerPath := filepath.Join(issueWsPath, "marker_file")
	data, err := os.ReadFile(markerPath)
	require.NoError(t, err, "marker_file should exist in the workspace")

	// The contents should be the workspace path (pwd output).
	writtenPath := strings.TrimSpace(string(data))
	// Resolve symlinks so /tmp vs /private/tmp differences on macOS don't fail.
	resolvedMarkerPath, _ := filepath.EvalSymlinks(writtenPath)
	resolvedExpected, _ := filepath.EvalSymlinks(issueWsPath)
	assert.Equal(t, resolvedExpected, resolvedMarkerPath, "hook cwd should be the issue workspace")
}

// TestBetweenTurns_StdoutTruncation verifies that hook stdout > 4 KiB is truncated
// and the truncation marker appears in the feedback section.
func TestBetweenTurns_StdoutTruncation(t *testing.T) {
	cfg := buildBetweenTurnsCfg(t)
	// Print 8 KiB of 'x' characters then exit non-zero.
	cfg.Hooks.BetweenTurns = "python3 -c \"print('x' * 8192, end='')\" ; exit 1"

	issue := activeIssue("bt-6", "BT-6")
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
		},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	require.True(t, ok)

	waitForTurnCount(t, rec, 2, 10*time.Second)
	cancel()

	prompts := rec.RecordedPrompts()
	require.GreaterOrEqual(t, len(prompts), 2, "expected at least 2 recorded turns")

	turn2Prompt := prompts[1].Prompt
	// Truncation marker must appear.
	assert.Contains(t, turn2Prompt, "[truncated, full output suppressed]", "truncation marker must appear")

	// The feedback section itself (between the fences) must be ≤ ~4 KiB + overhead.
	// We allow 4 KiB cap + ~200 bytes for the marker text + prompt boilerplate.
	const maxAllowed = 4*1024 + 512
	assert.LessOrEqual(t, len(turn2Prompt), maxAllowed,
		"total prompt length should stay within reasonable bounds of the 4 KiB cap")
}
