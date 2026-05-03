// Package agent_test provides a runtime-agnostic contract test suite for all
// registered runtimes (mock, codex, claude) per SPEC §10.7.
package agent_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	_ "github.com/openai/symphony/go/internal/agent/claude" // register claude runtime
	_ "github.com/openai/symphony/go/internal/agent/codex"  // register codex runtime
	"github.com/openai/symphony/go/internal/agent/internal/fakeagent"
	agentmock "github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// TestMain handles the re-exec fake-agent pattern. When
// SYMPHONY_FAKE_AGENT_MODE is set, the binary acts as the fake agent.
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

// codexHappyScript is a shared happy-path script for the Codex fake.
func codexHappyScript() []fakeagent.Directive {
	return []fakeagent.Directive{
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
				"thread": map[string]any{"id": "contract-thread"},
			},
		}},
		{ExpectStdinLine: "turn/start"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result": map[string]any{
				"turn": map[string]any{"id": "contract-turn"},
			},
		}},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"method":  "item/assistant/message",
			"params":  map[string]any{"content": "contract test event"},
		}},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params":  map[string]any{},
		}},
	}
}

// claudeHappyScript is a shared happy-path script for the Claude fake.
func claudeHappyScript() []fakeagent.Directive {
	return []fakeagent.Directive{
		{WriteStdoutJSONL: map[string]any{
			"type":       "system",
			"subtype":    "init",
			"session_id": "contract-claude-sess",
		}},
		{WriteStdoutJSONL: map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "contract test"}},
			},
		}},
		{WriteStdoutJSONL: map[string]any{
			"type":    "result",
			"subtype": "success",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"total_tokens":  15,
			},
		}},
	}
}

// runtimeCase describes one runtime under test.
type runtimeCase struct {
	name string
	cfg  func(t *testing.T) config.Config
}

func buildRuntimeCases(t *testing.T) []runtimeCase {
	t.Helper()
	bin := selfBinary(t)

	cases := []runtimeCase{
		{
			name: "mock",
			cfg: func(t *testing.T) config.Config {
				// The mock runtime is configured via agent.New which returns
				// the factory-registered default mock (no scripted events).
				// For contract tests that need a session we use the default mock.
				return config.Config{
					Agent: config.Agent{Runtime: "mock"},
				}
			},
		},
		{
			name: "codex",
			cfg: func(t *testing.T) config.Config {
				script := codexHappyScript()
				encoded, err := fakeagent.EncodeScript(script)
				require.NoError(t, err)
				cmd := "SYMPHONY_FAKE_AGENT_MODE=contract_codex SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded + " " + bin + " -test.run=^$"
				return config.Config{
					Agent: config.Agent{Runtime: "codex"},
					Codex: config.Codex{
						Command:        cmd,
						TurnTimeoutMs:  10000,
						ReadTimeoutMs:  5000,
						StallTimeoutMs: 30000,
					},
				}
			},
		},
		{
			name: "claude",
			cfg: func(t *testing.T) config.Config {
				script := claudeHappyScript()
				encoded, err := fakeagent.EncodeScript(script)
				require.NoError(t, err)
				cmd := "SYMPHONY_FAKE_AGENT_MODE=contract_claude SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded + " " + bin + " -test.run=^$"
				return config.Config{
					Agent: config.Agent{Runtime: "claude"},
					Claude: config.Claude{
						Command:        cmd,
						TurnTimeoutMs:  10000,
						ReadTimeoutMs:  5000,
						StallTimeoutMs: 30000,
					},
				}
			},
		},
	}
	return cases
}

// TestContract_StartSession verifies that StartSession returns a non-empty
// session ID for each runtime.
func TestContract_StartSession(t *testing.T) {
	for _, tc := range buildRuntimeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg(t)
			rt, err := agent.New(cfg)
			require.NoError(t, err)

			ctx := context.Background()
			ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
			sess, err := rt.StartSession(ctx, ws)
			require.NoError(t, err)
			assert.NotEmpty(t, sess.ID)

			require.NoError(t, rt.StopSession(ctx, sess))
		})
	}
}

// runtimeFactory builds a Runtime for the given case, allowing callers to
// inject a runtime directly (e.g. a pre-configured mock) when needed.
type runtimeFactory struct {
	name string
	rt   agent.Runtime
}

// buildRuntimesWithEvents builds runtimes that are guaranteed to emit at least
// one event during RunTurn.
func buildRuntimesWithEvents(t *testing.T) []runtimeFactory {
	t.Helper()
	bin := selfBinary(t)

	// For the mock we create one directly with scripted events instead of going
	// through agent.New (which creates a mock with no scripted events and thus
	// no callbacks).
	mockRT := agentmock.New(agentmock.MockOpts{
		Turns: []agentmock.TurnScript{
			{
				Status: agent.TurnCompleted,
				Events: []agent.Event{
					{Kind: agent.EventAssistantMessage, Payload: "hello"},
				},
			},
		},
	})

	script := codexHappyScript()
	codexEncoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)
	codexCmd := "SYMPHONY_FAKE_AGENT_MODE=contract_codex_cb SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + codexEncoded + " " + bin + " -test.run=^$"
	codexCfg := config.Config{
		Agent: config.Agent{Runtime: "codex"},
		Codex: config.Codex{
			Command:       codexCmd,
			TurnTimeoutMs: 10000, ReadTimeoutMs: 5000, StallTimeoutMs: 30000,
		},
	}
	codexRT, err := agent.New(codexCfg)
	require.NoError(t, err)

	script = claudeHappyScript()
	claudeEncoded, err := fakeagent.EncodeScript(script)
	require.NoError(t, err)
	claudeCmd := "SYMPHONY_FAKE_AGENT_MODE=contract_claude_cb SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + claudeEncoded + " " + bin + " -test.run=^$"
	claudeCfg := config.Config{
		Agent: config.Agent{Runtime: "claude"},
		Claude: config.Claude{
			Command:       claudeCmd,
			TurnTimeoutMs: 10000, ReadTimeoutMs: 5000, StallTimeoutMs: 30000,
		},
	}
	claudeRT, err := agent.New(claudeCfg)
	require.NoError(t, err)

	return []runtimeFactory{
		{name: "mock", rt: mockRT},
		{name: "codex", rt: codexRT},
		{name: "claude", rt: claudeRT},
	}
}

// TestContract_RunTurn_InvokesCallback verifies that RunTurn invokes the event
// callback at least once and returns a non-empty SessionID.
func TestContract_RunTurn_InvokesCallback(t *testing.T) {
	for _, tc := range buildRuntimesWithEvents(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
			sess, err := tc.rt.StartSession(ctx, ws)
			require.NoError(t, err)

			var callbackCalled int
			cb := func(ev agent.Event) {
				callbackCalled++
			}

			result, err := tc.rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, cb)
			require.NoError(t, err)
			assert.NotEmpty(t, result.SessionID)
			assert.Greater(t, callbackCalled, 0, "callback should have been called at least once")

			tc.rt.StopSession(ctx, sess) //nolint:errcheck
		})
	}
}

// TestContract_CancelledContext verifies that a cancelled context propagates
// and RunTurn returns within read_timeout_ms.
func TestContract_CancelledContext(t *testing.T) {
	// For cancel test, use a script that stalls indefinitely so we can cancel.
	cancelScriptCodex := []fakeagent.Directive{
		{ExpectStdinLine: "initialize"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{},
		}},
		{ExpectStdinLine: "initialized"},
		{ExpectStdinLine: "thread/start"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"result":  map[string]any{"thread": map[string]any{"id": "cancel-thread"}},
		}},
		{ExpectStdinLine: "turn/start"},
		{WriteStdoutJSONL: map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"result":  map[string]any{"turn": map[string]any{"id": "cancel-turn"}},
		}},
		// Stall: no more output until cancelled.
		{WaitMs: 30000},
	}

	cancelScriptClaude := []fakeagent.Directive{
		// Stall immediately.
		{WaitMs: 30000},
	}

	bin := selfBinary(t)

	cases := []struct {
		name   string
		cfg    config.Config
		script []fakeagent.Directive
		mode   string
	}{
		{
			name:   "codex",
			script: cancelScriptCodex,
			mode:   "contract_cancel_codex",
		},
		{
			name:   "claude",
			script: cancelScriptClaude,
			mode:   "contract_cancel_claude",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := fakeagent.EncodeScript(tc.script)
			require.NoError(t, err)
			cmd := "SYMPHONY_FAKE_AGENT_MODE=" + tc.mode + " SYMPHONY_FAKE_AGENT_SCRIPT_B64=" + encoded + " " + bin + " -test.run=^$"

			var cfg config.Config
			switch tc.name {
			case "codex":
				cfg = config.Config{
					Agent: config.Agent{Runtime: "codex"},
					Codex: config.Codex{
						Command:       cmd,
						TurnTimeoutMs: 10000, ReadTimeoutMs: 300, StallTimeoutMs: 30000,
					},
				}
			case "claude":
				cfg = config.Config{
					Agent: config.Agent{Runtime: "claude"},
					Claude: config.Claude{
						Command:       cmd,
						TurnTimeoutMs: 10000, ReadTimeoutMs: 300, StallTimeoutMs: 30000,
					},
				}
			}

			rt, err := agent.New(cfg)
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(context.Background())

			ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}

			if tc.name == "codex" {
				// Start session first (happens before turn for codex).
				sess, err := rt.StartSession(ctx, ws)
				require.NoError(t, err)

				// Cancel the context just before RunTurn is processing.
				go func() {
					time.Sleep(50 * time.Millisecond)
					cancel()
				}()

				start := time.Now()
				result, _ := rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, nil)
				elapsed := time.Since(start)

				assert.Less(t, elapsed, 2*time.Second, "should return quickly after context cancel")
				assert.NotEqual(t, agent.TurnCompleted, result.Status)
				rt.StopSession(context.Background(), sess) //nolint:errcheck
			} else {
				// Claude: start session, then cancel context during RunTurn.
				sess, err := rt.StartSession(ctx, ws)
				require.NoError(t, err)

				go func() {
					time.Sleep(50 * time.Millisecond)
					cancel()
				}()

				start := time.Now()
				result, _ := rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, nil)
				elapsed := time.Since(start)

				assert.Less(t, elapsed, 2*time.Second, "should return quickly after context cancel")
				assert.NotEqual(t, agent.TurnCompleted, result.Status)
				rt.StopSession(context.Background(), sess) //nolint:errcheck
			}
		})
	}
}

// TestContract_DoubleStopSession verifies that calling StopSession twice is
// a no-op (does not panic or return an error).
func TestContract_DoubleStopSession(t *testing.T) {
	for _, tc := range buildRuntimeCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg(t)
			rt, err := agent.New(cfg)
			require.NoError(t, err)

			ctx := context.Background()
			ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
			sess, err := rt.StartSession(ctx, ws)
			require.NoError(t, err)

			require.NoError(t, rt.StopSession(ctx, sess))
			require.NoError(t, rt.StopSession(ctx, sess))
		})
	}
}
