package claude_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/claude"
	"github.com/openai/symphony/go/internal/agent/internal/fakeagent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// TestMain handles the re-exec fake-agent pattern.
func TestMain(m *testing.M) {
	mode := os.Getenv("SYMPHONY_FAKE_AGENT_MODE")
	if mode != "" {
		fakeagent.Run(mode)
		return
	}
	os.Exit(m.Run())
}

func selfBinary(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	require.NoError(t, err)
	return bin
}

func newTestCfg(t *testing.T, mode string, script []fakeagent.Directive) config.Config {
	t.Helper()
	encoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)
	bin := selfBinary(t)
	cmd := bin + " -test.run=^$"
	envCmd := "SYMPHONY_FAKE_AGENT_MODE=" + mode +
		" SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded +
		" " + cmd
	return config.Config{
		Claude: config.Claude{
			Command:        envCmd,
			TurnTimeoutMs:  10000,
			ReadTimeoutMs:  5000,
			StallTimeoutMs: 30000,
		},
		Agent: config.Agent{
			Runtime: "claude",
		},
	}
}

// happyPathScript returns a script that emits system/init, assistant message,
// and result/success events.
func happyPathScript() []fakeagent.Directive {
	return []fakeagent.Directive{
		// Claude doesn't need to read the prompt first (it just reads from stdin),
		// but we simulate it by not requiring an explicit stdin read here.
		// Emit system/init.
		{WriteStdoutJSONL: map[string]any{
			"type":       "system",
			"subtype":    "init",
			"session_id": "sess-abc123",
		}},
		// Emit assistant message.
		{WriteStdoutJSONL: map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "Hello from Claude"}},
			},
		}},
		// Emit result/success.
		{WriteStdoutJSONL: map[string]any{
			"type":    "result",
			"subtype": "success",
			"usage": map[string]any{
				"input_tokens":  200,
				"output_tokens": 80,
				"total_tokens":  280,
			},
		}},
	}
}

func TestClaude_StartSession(t *testing.T) {
	script := happyPathScript()
	cfg := newTestCfg(t, "claude_start", script)
	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)

	require.NoError(t, rt.StopSession(ctx, sess))
}

func TestClaude_RunTurn_HappyPath(t *testing.T) {
	script := happyPathScript()
	cfg := newTestCfg(t, "claude_turn", script)
	rt := claude.New(cfg)

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
	// Session ID should be updated to the one from the system/init event.
	assert.Equal(t, "sess-abc123", result.SessionID)

	assert.Equal(t, 200, result.Tokens.InputTokens)
	assert.Equal(t, 80, result.Tokens.OutputTokens)
	assert.Equal(t, 280, result.Tokens.TotalTokens)

	// Should have received EventThreadStarted, EventAssistantMessage, EventTurnCompleted.
	require.GreaterOrEqual(t, len(events), 3)

	var kinds []agent.EventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	assert.Contains(t, kinds, agent.EventThreadStarted)
	assert.Contains(t, kinds, agent.EventAssistantMessage)
	assert.Contains(t, kinds, agent.EventTurnCompleted)

	require.NoError(t, rt.StopSession(ctx, sess))
}

func TestClaude_RunTurn_Failure(t *testing.T) {
	script := []fakeagent.Directive{
		{WriteStdoutJSONL: map[string]any{
			"type":    "result",
			"subtype": "error_during_execution",
		}},
	}

	cfg := newTestCfg(t, "claude_fail", script)
	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	result, err := rt.RunTurn(ctx, sess, "fail please", domain.Issue{ID: "1"}, nil)
	assert.Equal(t, agent.TurnFailed, result.Status)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error_during_execution")

	rt.StopSession(ctx, sess) //nolint:errcheck
}

func TestClaude_ReadTimeout(t *testing.T) {
	// Script stalls without writing anything.
	script := []fakeagent.Directive{
		{WaitMs: 3000},
	}

	cfg := newTestCfg(t, "claude_timeout", script)
	cfg.Claude.ReadTimeoutMs = 200
	// Re-encode with short timeout.
	encoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)
	bin := selfBinary(t)
	cfg.Claude.Command = "SYMPHONY_FAKE_AGENT_MODE=claude_timeout SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded + " " + bin + " -test.run=^$"

	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	start := time.Now()
	_, err = rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, nil)
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestClaude_SystemInit_UpdatesSessionID(t *testing.T) {
	script := []fakeagent.Directive{
		{WriteStdoutJSONL: map[string]any{
			"type":       "system",
			"subtype":    "init",
			"session_id": "sess-123",
		}},
		{WriteStdoutJSONL: map[string]any{
			"type":    "result",
			"subtype": "success",
		}},
	}

	cfg := newTestCfg(t, "claude_init", script)
	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	var initEvent *agent.Event
	cb := func(ev agent.Event) {
		if ev.Kind == agent.EventThreadStarted {
			ev2 := ev
			initEvent = &ev2
		}
	}

	result, err := rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, cb)
	require.NoError(t, err)
	assert.Equal(t, "sess-123", result.SessionID)
	require.NotNil(t, initEvent)
	assert.Equal(t, "sess-123", initEvent.SessionID)

	rt.StopSession(ctx, sess) //nolint:errcheck
}

func TestClaude_DoubleStopSession_NoOp(t *testing.T) {
	script := happyPathScript()
	cfg := newTestCfg(t, "claude_stop", script)
	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	require.NoError(t, rt.StopSession(ctx, sess))
	require.NoError(t, rt.StopSession(ctx, sess))
}

func TestClaude_PromptWrittenToStdin(t *testing.T) {
	// The fake agent reads one line from stdin and echoes it back as an
	// assistant message, then sends result/success.
	script := []fakeagent.Directive{
		// Wait for a line on stdin (the prompt JSON).
		{ExpectStdinLine: "user"},
		// Echo it back as assistant.
		{WriteStdoutJSONL: map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "received prompt"}},
			},
		}},
		{WriteStdoutJSONL: map[string]any{
			"type":    "result",
			"subtype": "success",
		}},
	}

	cfg := newTestCfg(t, "claude_prompt", script)
	rt := claude.New(cfg)

	ctx := context.Background()
	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	var events []agent.Event
	result, err := rt.RunTurn(ctx, sess, "my prompt text", domain.Issue{ID: "1"}, func(ev agent.Event) {
		events = append(events, ev)
	})
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, result.Status)

	// The assistant message event should have been received.
	var hasAssistant bool
	for _, ev := range events {
		if ev.Kind == agent.EventAssistantMessage {
			hasAssistant = true
		}
	}
	assert.True(t, hasAssistant, "should have received EventAssistantMessage")

	rt.StopSession(ctx, sess) //nolint:errcheck
}
