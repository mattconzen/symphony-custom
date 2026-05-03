package codex_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/codex"
	"github.com/openai/symphony/go/internal/agent/internal/fakeagent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// TestMain handles the re-exec fake-agent pattern. When
// SYMPHONY_FAKE_AGENT_MODE is set, the binary acts as the fake agent.
func TestMain(m *testing.M) {
	mode := os.Getenv("SYMPHONY_FAKE_AGENT_MODE")
	if mode != "" {
		fakeagent.Run(mode)
		// fakeagent.Run always exits; this line is unreachable.
		return
	}
	os.Exit(m.Run())
}

// selfBinary returns the path to the currently running test binary.
func selfBinary(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	require.NoError(t, err)
	return bin
}

// newTestCfg returns a config.Config with the Codex command set to the test
// binary acting as a fake agent, with the given mode and script.
func newTestCfg(t *testing.T, mode string, script []fakeagent.Directive) config.Config {
	t.Helper()
	encoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)

	bin := selfBinary(t)
	// The command is the test binary invoked with -test.run=^$ (no real tests)
	// and env vars set via the Cmd.Env after cmd.Start.
	// We use a shell command that sets the env and calls the binary.
	cmd := bin + " -test.run=^$"

	cfg := config.Config{
		Codex: config.Codex{
			Command:        cmd,
			TurnTimeoutMs:  10000,
			ReadTimeoutMs:  5000,
			StallTimeoutMs: 30000,
		},
		Agent: config.Agent{
			Runtime: "codex",
		},
	}

	// We need to inject env vars into the subprocess. We do this by wrapping
	// the command in a shell that exports the vars.
	envCmd := "SYMPHONY_FAKE_AGENT_MODE=" + mode +
		" SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded +
		" " + cmd
	cfg.Codex.Command = envCmd

	return cfg
}

// happyPathScript returns a fake-agent script that handles the full
// initialize -> thread/start -> turn/start -> turn/completed flow.
func happyPathScript(t *testing.T) []fakeagent.Directive {
	t.Helper()
	return []fakeagent.Directive{
		// Read initialize request.
		{ExpectStdinLine: "initialize"},
		// Respond to initialize.
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"capabilities": map[string]any{}},
		}},
		// Read initialized notification + thread/start request.
		{ExpectStdinLine: "initialized"},
		{ExpectStdinLine: "thread/start"},
		// Respond to thread/start with a thread_id.
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"result": map[string]any{
				"thread": map[string]any{"id": "test-thread-id"},
			},
		}},
		// Read turn/start request.
		{ExpectStdinLine: "turn/start"},
		// Respond to turn/start with a turn_id.
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result": map[string]any{
				"turn": map[string]any{"id": "test-turn-id"},
			},
		}},
		// Emit a turn event (assistant message notification).
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"method":  "item/assistant/message",
			"params":  map[string]any{"content": "Hello from codex"},
		}},
		// Emit turn/completed.
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params": map[string]any{
				"usage": map[string]any{
					"input_tokens":  100,
					"output_tokens": 50,
					"total_tokens":  150,
				},
			},
		}},
	}
}

func TestCodex_StartSession_HappyPath(t *testing.T) {
	script := happyPathScript(t)
	// Only use the initialize + thread/start part.
	cfg := newTestCfg(t, "codex_start", script)
	rt := codex.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)

	// Clean up.
	require.NoError(t, rt.StopSession(ctx, sess))
}

func TestCodex_RunTurn_HappyPath(t *testing.T) {
	script := happyPathScript(t)
	cfg := newTestCfg(t, "codex_turn", script)
	rt := codex.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	var events []agent.Event
	cb := func(ev agent.Event) {
		events = append(events, ev)
	}

	result, err := rt.RunTurn(ctx, sess, "do the thing", domain.Issue{ID: "1", Identifier: "X-1", Title: "Test"}, cb)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, result.Status)
	assert.NotEmpty(t, result.SessionID)
	assert.Equal(t, 100, result.Tokens.InputTokens)
	assert.Equal(t, 50, result.Tokens.OutputTokens)
	assert.Equal(t, 150, result.Tokens.TotalTokens)

	// Should have received at least EventThreadStarted + EventAssistantMessage + EventTurnCompleted.
	require.GreaterOrEqual(t, len(events), 2)

	require.NoError(t, rt.StopSession(ctx, sess))
}

func TestCodex_RunTurn_Failure(t *testing.T) {
	script := []fakeagent.Directive{
		{ExpectStdinLine: "initialize"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{},
		}},
		{ExpectStdinLine: "initialized"},
		{ExpectStdinLine: "thread/start"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"result": map[string]any{
				"thread": map[string]any{"id": "fail-thread"},
			},
		}},
		{ExpectStdinLine: "turn/start"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result": map[string]any{
				"turn": map[string]any{"id": "fail-turn"},
			},
		}},
		// Emit turn/failed notification.
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/failed",
			"params":  map[string]any{"reason": "something went wrong"},
		}},
	}

	cfg := newTestCfg(t, "codex_fail", script)
	rt := codex.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	result, err := rt.RunTurn(ctx, sess, "fail please", domain.Issue{ID: "1"}, nil)
	assert.Equal(t, agent.TurnFailed, result.Status)
	assert.Error(t, err)

	rt.StopSession(ctx, sess) //nolint:errcheck
}

func TestCodex_ReadTimeout(t *testing.T) {
	// Script stalls after initialize (never responds to thread/start).
	script := []fakeagent.Directive{
		{ExpectStdinLine: "initialize"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{},
		}},
		{ExpectStdinLine: "initialized"},
		{ExpectStdinLine: "thread/start"},
		// Stall: wait longer than ReadTimeoutMs.
		{WaitMs: 3000},
	}

	cfg := newTestCfg(t, "codex_timeout", script)
	cfg.Codex.ReadTimeoutMs = 200 // Short timeout for test speed.
	// Re-encode with the short timeout.
	encoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)
	bin := selfBinary(t)
	cfg.Codex.Command = "SYMPHONY_FAKE_AGENT_MODE=codex_timeout SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded + " " + bin + " -test.run=^$"

	rt := codex.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}

	// StartSession should fail due to read timeout during thread/start.
	start := time.Now()
	_, err = rt.StartSession(ctx, ws)
	elapsed := time.Since(start)

	assert.Error(t, err)
	// Should fail well within 2 seconds.
	assert.Less(t, elapsed, 2*time.Second)
}

func TestCodex_DoubleStopSession_NoOp(t *testing.T) {
	script := happyPathScript(t)
	cfg := newTestCfg(t, "codex_stop", script)
	rt := codex.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	require.NoError(t, rt.StopSession(ctx, sess))
	require.NoError(t, rt.StopSession(ctx, sess)) // Must be idempotent.
}
