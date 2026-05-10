// Package claudesdk implements the in-process Anthropic Messages API agent
// runtime per SPEC §10.9. Unlike the CLI-based `claude` runtime, this one
// keeps the conversation history in memory across Symphony turns and runs
// tool calls in-process against the per-issue workspace.
package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

func init() {
	agent.RegisterClaudeSDKFactory(func(cfg config.Config) (agent.Runtime, error) {
		return New(cfg, nil)
	})
}

// Runtime is the claude_sdk implementation of agent.Runtime.
type Runtime struct {
	cfg    config.Config
	client *apiClient
	tools  *toolRegistry
}

// New constructs a Runtime. Pass a non-nil httpClient to redirect API calls
// (tests use httptest.NewServer's client). Production callers should pass
// nil and the runtime will use a stdlib client.
func New(cfg config.Config, httpClient *http.Client) (*Runtime, error) {
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	if cfg.ClaudeSDK.APIKey == "" {
		return nil, fmt.Errorf("claudesdk: API key is empty (set %s)", cfg.ClaudeSDK.APIKeyEnv)
	}
	return &Runtime{
		cfg:    cfg,
		client: newAPIClient(cfg.ClaudeSDK.APIKey, cfg.ClaudeSDK.BaseURL, cfg.ClaudeSDK.Model, httpClient),
		tools:  newToolRegistry(defaultTools()...),
	}, nil
}

// NewWithTools constructs a Runtime that overrides the default tool set.
// Used by tests to inject deterministic fake tools.
func NewWithTools(cfg config.Config, httpClient *http.Client, tools ...Tool) (*Runtime, error) {
	rt, err := New(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	rt.tools = newToolRegistry(tools...)
	return rt, nil
}

// sessionImpl carries the multi-turn state across RunTurn calls. messages
// is the full Anthropic-shape conversation history; workspace is the
// resolved workspace path used for tool execution.
type sessionImpl struct {
	mu        sync.Mutex
	messages  []message
	workspace string
	closed    atomic.Bool
}

// StartSession allocates a new conversation. No network I/O happens here.
func (r *Runtime) StartSession(_ context.Context, ws domain.Workspace) (agent.Session, error) {
	impl := &sessionImpl{workspace: ws.Path}
	id := fmt.Sprintf("claude-sdk-%x", time.Now().UnixNano())
	return agent.Session{ID: id, Impl: impl}, nil
}

// StopSession marks the session closed. No resources to release.
func (r *Runtime) StopSession(_ context.Context, sess agent.Session) error {
	impl, ok := sess.Impl.(*sessionImpl)
	if !ok || impl == nil {
		return nil
	}
	impl.closed.Store(true)
	return nil
}

// RunTurn appends the prompt to the conversation history, then iterates:
// call Messages API → if response contains tool_use blocks, execute each
// and append tool_result blocks → repeat. Returns when the model emits no
// further tool_use blocks (natural stop) or when the iteration cap or
// turn timeout is hit.
func (r *Runtime) RunTurn(ctx context.Context, sess agent.Session, prompt string, _ domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	impl, ok := sess.Impl.(*sessionImpl)
	if !ok || impl == nil {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("claudesdk: invalid session impl")
	}
	if impl.closed.Load() {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("claudesdk: session is closed")
	}

	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.ClaudeSDK.TurnTimeoutMs)*time.Millisecond)
	defer cancel()

	emit := func(kind agent.EventKind, payload any) {
		if cb != nil {
			cb(agent.Event{
				Kind:      kind,
				Timestamp: time.Now().UTC(),
				SessionID: sess.ID,
				Payload:   payload,
			})
		}
	}

	impl.mu.Lock()
	impl.messages = append(impl.messages, message{
		Role:    "user",
		Content: []contentBlock{{Type: "text", Text: prompt}},
	})
	impl.mu.Unlock()

	emit(agent.EventThreadStarted, map[string]any{"session_id": sess.ID})

	var totalIn, totalOut int
	maxIter := r.cfg.ClaudeSDK.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}

	for iter := 0; iter < maxIter; iter++ {
		if turnCtx.Err() != nil {
			return agent.TurnResult{
				SessionID: sess.ID,
				Status:    agent.TurnCancelled,
				Tokens:    agent.TokenUsage{InputTokens: totalIn, OutputTokens: totalOut, TotalTokens: totalIn + totalOut},
			}, turnCtx.Err()
		}

		impl.mu.Lock()
		req := messagesRequest{
			Model:     r.cfg.ClaudeSDK.Model,
			MaxTokens: r.cfg.ClaudeSDK.MaxTokens,
			Messages:  cloneMessages(impl.messages),
			Tools:     r.tools.defs(),
			System:    r.cfg.ClaudeSDK.SystemPrompt,
		}
		impl.mu.Unlock()

		resp, err := r.client.createMessage(turnCtx, req)
		if err != nil {
			if turnCtx.Err() != nil {
				emit(agent.EventTurnCancelled, map[string]any{"err": err.Error()})
				return agent.TurnResult{
					SessionID: sess.ID,
					Status:    agent.TurnCancelled,
					Tokens:    agent.TokenUsage{InputTokens: totalIn, OutputTokens: totalOut, TotalTokens: totalIn + totalOut},
				}, turnCtx.Err()
			}
			emit(agent.EventTurnFailed, map[string]any{"err": err.Error()})
			return agent.TurnResult{
				SessionID: sess.ID,
				Status:    agent.TurnFailed,
				Tokens:    agent.TokenUsage{InputTokens: totalIn, OutputTokens: totalOut, TotalTokens: totalIn + totalOut},
				Err:       err,
			}, err
		}

		totalIn += resp.Usage.InputTokens
		totalOut += resp.Usage.OutputTokens

		impl.mu.Lock()
		impl.messages = append(impl.messages, message{Role: "assistant", Content: resp.Content})
		impl.mu.Unlock()

		// Surface assistant content as events so the orchestrator transcript
		// captures it without re-parsing the message history.
		var toolUses []contentBlock
		for _, blk := range resp.Content {
			switch blk.Type {
			case "text":
				emit(agent.EventAssistantMessage, blk.Text)
			case "tool_use":
				toolUses = append(toolUses, blk)
				emit(agent.EventToolCall, map[string]any{
					"name":  blk.Name,
					"id":    blk.ID,
					"input": json.RawMessage(blk.Input),
				})
			}
		}

		if len(toolUses) == 0 {
			emit(agent.EventTurnCompleted, map[string]any{"stop_reason": resp.StopReason})
			return agent.TurnResult{
				SessionID: sess.ID,
				Status:    agent.TurnCompleted,
				Tokens:    agent.TokenUsage{InputTokens: totalIn, OutputTokens: totalOut, TotalTokens: totalIn + totalOut},
			}, nil
		}

		results := make([]contentBlock, 0, len(toolUses))
		for _, tu := range toolUses {
			out, isErr := r.tools.runTool(turnCtx, impl.workspace, tu.Name, tu.Input)
			results = append(results, contentBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   out,
				IsError:   isErr,
			})
			emit(agent.EventToolResult, map[string]any{
				"tool_use_id": tu.ID,
				"is_error":    isErr,
				"output":      out,
			})
		}

		impl.mu.Lock()
		impl.messages = append(impl.messages, message{Role: "user", Content: results})
		impl.mu.Unlock()
	}

	err := fmt.Errorf("claudesdk: max_iterations (%d) reached without natural stop", maxIter)
	emit(agent.EventTurnFailed, map[string]any{"err": err.Error()})
	return agent.TurnResult{
		SessionID: sess.ID,
		Status:    agent.TurnFailed,
		Tokens:    agent.TokenUsage{InputTokens: totalIn, OutputTokens: totalOut, TotalTokens: totalIn + totalOut},
		Err:       err,
	}, err
}

// cloneMessages returns a shallow copy of the message slice so concurrent
// appends from another RunTurn cannot mutate an in-flight request body.
func cloneMessages(src []message) []message {
	out := make([]message, len(src))
	copy(out, src)
	return out
}
