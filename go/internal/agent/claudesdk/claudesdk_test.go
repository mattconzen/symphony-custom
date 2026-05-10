package claudesdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// fakeAPI returns a series of canned messagesResponse values for sequential
// POST /v1/messages requests. requestCount counts how many requests landed
// so tests can assert the multi-turn loop fired the expected number of
// API calls.
type fakeAPI struct {
	responses    []messagesResponse
	requestCount int32
	captured     []messagesRequest
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req messagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		idx := int(atomic.AddInt32(&f.requestCount, 1)) - 1
		f.captured = append(f.captured, req)
		if idx >= len(f.responses) {
			http.Error(w, `{"type":"error","error":{"type":"overflow","message":"no more canned responses"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.responses[idx])
	}
}

func newTestRuntime(t *testing.T, api *fakeAPI) (*Runtime, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk", MaxTurns: 5},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "test-key", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 1024, TurnTimeoutMs: 5_000, MaxIterations: 10},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)
	return rt, srv
}

func TestRuntime_SingleTurnNoTools(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{{
			ID:         "msg_1",
			Role:       "assistant",
			Content:    []contentBlock{{Type: "text", Text: "done"}},
			StopReason: "end_turn",
			Usage:      tokenUsage{InputTokens: 10, OutputTokens: 4},
		}},
	}
	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	var seen []agent.Event
	cb := func(ev agent.Event) { seen = append(seen, ev) }

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, cb)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)
	assert.Equal(t, 10, res.Tokens.InputTokens)
	assert.Equal(t, 4, res.Tokens.OutputTokens)
	assert.Equal(t, 14, res.Tokens.TotalTokens)

	// At least one of session_started + assistant_message + turn_completed.
	kinds := map[agent.EventKind]int{}
	for _, ev := range seen {
		kinds[ev.Kind]++
	}
	assert.GreaterOrEqual(t, kinds[agent.EventAssistantMessage], 1)
	assert.Equal(t, 1, kinds[agent.EventTurnCompleted])
}

func TestRuntime_MultiTurnPersistsHistory(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{
			{Content: []contentBlock{{Type: "text", Text: "first"}}, Usage: tokenUsage{InputTokens: 5, OutputTokens: 2}, StopReason: "end_turn"},
			{Content: []contentBlock{{Type: "text", Text: "second"}}, Usage: tokenUsage{InputTokens: 7, OutputTokens: 3}, StopReason: "end_turn"},
		},
	}
	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	_, err = rt.RunTurn(context.Background(), sess, "turn 1", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	_, err = rt.RunTurn(context.Background(), sess, "turn 2", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)

	require.Len(t, api.captured, 2)
	// Second request must carry prior history: user, assistant, user.
	second := api.captured[1]
	require.Len(t, second.Messages, 3)
	assert.Equal(t, "user", second.Messages[0].Role)
	assert.Equal(t, "assistant", second.Messages[1].Role)
	assert.Equal(t, "user", second.Messages[2].Role)
	assert.Equal(t, "turn 2", second.Messages[2].Content[0].Text)
}

func TestRuntime_ToolCallLoop(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0o644))

	api := &fakeAPI{
		responses: []messagesResponse{
			{Content: []contentBlock{
				{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"hello.txt"}`)},
			}, StopReason: "tool_use"},
			{Content: []contentBlock{{Type: "text", Text: "found world"}}, StopReason: "end_turn"},
		},
	}
	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: dir, Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	var seen []agent.Event
	cb := func(ev agent.Event) { seen = append(seen, ev) }

	res, err := rt.RunTurn(context.Background(), sess, "read hello.txt", domain.Issue{ID: "1"}, cb)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)
	assert.Equal(t, int32(2), api.requestCount, "loop should re-send after tool_use")

	var toolResultSeen bool
	for _, ev := range seen {
		if ev.Kind == agent.EventToolResult {
			payload, _ := ev.Payload.(map[string]any)
			if out, _ := payload["output"].(string); strings.Contains(out, "world") {
				toolResultSeen = true
			}
		}
	}
	assert.True(t, toolResultSeen, "tool_result event should carry the file contents")
}

func TestRuntime_APIErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"rate_limit","message":"too many"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "test-key", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 1000, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	require.Error(t, err)
	assert.Equal(t, agent.TurnFailed, res.Status)
}

func TestRuntime_MaxIterationsCap(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("y"), 0o644))

	// Every response asks for a Read — infinite loop without the cap.
	resp := messagesResponse{
		Content: []contentBlock{
			{Type: "tool_use", ID: "tu_loop", Name: "Read", Input: json.RawMessage(`{"path":"x.txt"}`)},
		},
		StopReason: "tool_use",
	}
	api := &fakeAPI{}
	for i := 0; i < 30; i++ {
		api.responses = append(api.responses, resp)
	}

	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: dir, Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "loop", domain.Issue{ID: "1"}, nil)
	require.Error(t, err)
	assert.Equal(t, agent.TurnFailed, res.Status)
	assert.Contains(t, err.Error(), "max_iterations")
}

func TestRuntime_CancelledContextStopsLoop(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{
			{Content: []contentBlock{
				{Type: "tool_use", ID: "tu_1", Name: "Bash", Input: json.RawMessage(`{"command":"sleep 5"}`)},
			}, StopReason: "tool_use"},
		},
	}
	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := rt.RunTurn(ctx, sess, "go", domain.Issue{ID: "1"}, nil)
	require.Error(t, err)
	assert.Equal(t, agent.TurnCancelled, res.Status)
}

func TestRuntime_RegistersFactory(t *testing.T) {
	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: "http://example.invalid", MaxTokens: 256, TurnTimeoutMs: 1000, MaxIterations: 5},
	}
	rt, err := agent.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, rt)
}
