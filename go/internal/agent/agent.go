// Package agent defines the Runtime interface and factory for coding-agent
// runtimes per SPEC §10.7.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// RegisterMockFactory registers the mock runtime factory. This is called by
// the agent/mock package's init() to avoid import cycles.
var RegisterMockFactory func(fn func() Runtime)

// mockFactory holds the mock runtime constructor registered by the mock package.
var mockFactory func() Runtime

// RegisterCodexFactory registers the Codex runtime factory. This is called by
// the agent/codex package's init() to avoid import cycles.
var RegisterCodexFactory func(fn func(config.Config) (Runtime, error))

// codexFactory holds the Codex runtime constructor registered by the codex package.
var codexFactory func(config.Config) (Runtime, error)

// RegisterClaudeFactory registers the Claude runtime factory. This is called by
// the agent/claude package's init() to avoid import cycles.
var RegisterClaudeFactory func(fn func(config.Config) (Runtime, error))

// claudeFactory holds the Claude runtime constructor registered by the claude package.
var claudeFactory func(config.Config) (Runtime, error)

// RegisterClaudeSDKFactory registers the in-process Anthropic-SDK runtime
// factory. Called by agent/claudesdk's init() to keep the import graph
// acyclic.
var RegisterClaudeSDKFactory func(fn func(config.Config) (Runtime, error))

// claudeSDKFactory holds the claude_sdk runtime constructor registered by the
// claudesdk package.
var claudeSDKFactory func(config.Config) (Runtime, error)

func init() {
	RegisterMockFactory = func(fn func() Runtime) {
		mockFactory = fn
	}
	RegisterCodexFactory = func(fn func(config.Config) (Runtime, error)) {
		codexFactory = fn
	}
	RegisterClaudeFactory = func(fn func(config.Config) (Runtime, error)) {
		claudeFactory = fn
	}
	RegisterClaudeSDKFactory = func(fn func(config.Config) (Runtime, error)) {
		claudeSDKFactory = fn
	}
}

// TurnStatus indicates the outcome of a RunTurn call.
type TurnStatus int

const (
	TurnCompleted TurnStatus = iota
	TurnFailed
	TurnCancelled
)

// TokenUsage holds token counts from a turn.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// Session holds the opaque session state for one agent subprocess.
type Session struct {
	ID   string
	Impl any
}

// TurnResult is the outcome of a single RunTurn call.
type TurnResult struct {
	SessionID string
	Status    TurnStatus
	Tokens    TokenUsage
	Err       error
}

// EventKind identifies the type of an agent event.
type EventKind string

const (
	EventThreadStarted    EventKind = "session_started"
	EventStartupFailed    EventKind = "startup_failed"
	EventAssistantMessage EventKind = "assistant_message"
	EventToolCall         EventKind = "tool_call"
	EventToolResult       EventKind = "tool_result"
	EventTurnCompleted    EventKind = "turn_completed"
	EventTurnFailed       EventKind = "turn_failed"
	EventTurnCancelled    EventKind = "turn_cancelled"
	EventNotification     EventKind = "notification"
	EventOtherMessage     EventKind = "other_message"
	EventMalformed        EventKind = "malformed"
	EventPRLink           EventKind = "pr_link"
)

// PRLinkPayload is the payload for EventPRLink events. URL is the canonical
// https://github.com/<owner>/<repo>/pull/<n> form; Owner/Repo/Number are the
// parsed components.
type PRLinkPayload struct {
	URL    string
	Owner  string
	Repo   string
	Number int
}

// Event is an agent runtime event emitted during a turn.
type Event struct {
	Kind      EventKind
	Timestamp time.Time
	SessionID string
	Payload   any
}

// EventCallback is a function called with each agent event.
type EventCallback func(Event)

// Runtime is the interface for coding-agent runtimes per SPEC §10.7.
type Runtime interface {
	StartSession(ctx context.Context, ws domain.Workspace) (Session, error)
	RunTurn(ctx context.Context, sess Session, prompt string, issue domain.Issue, cb EventCallback) (TurnResult, error)
	StopSession(ctx context.Context, sess Session) error
}

// NewForRole returns a Runtime configured for one pipeline role. The
// role-supplied Runtime + MaxTurns override the cfg defaults; everything
// else (codex / claude / claude_sdk blocks) is inherited so an operator
// only has to repeat the bits that differ per role.
func NewForRole(cfg config.Config, role config.PipelineRole) (Runtime, error) {
	if role.Runtime == "" {
		return nil, fmt.Errorf("agent.pipeline[%s]: runtime is required", role.Role)
	}
	scoped := cfg
	scoped.Agent.Runtime = role.Runtime
	if role.MaxTurns > 0 {
		scoped.Agent.MaxTurns = role.MaxTurns
	}
	return New(scoped)
}

// New returns a Runtime for the configured agent runtime. For Phase 1,
// "mock" is supported (when the mock package is imported). Codex and Claude
// are added in later phases.
func New(cfg config.Config) (Runtime, error) {
	switch cfg.Agent.Runtime {
	case "mock":
		if mockFactory == nil {
			return nil, fmt.Errorf("mock runtime not registered: import agent/mock with a blank import")
		}
		return mockFactory(), nil
	case "codex":
		if codexFactory == nil {
			return nil, fmt.Errorf("codex runtime not registered (import _ \"github.com/openai/symphony/go/internal/agent/codex\")")
		}
		return codexFactory(cfg)
	case "claude":
		if claudeFactory == nil {
			return nil, fmt.Errorf("claude runtime not registered (import _ \"github.com/openai/symphony/go/internal/agent/claude\")")
		}
		return claudeFactory(cfg)
	case "claude_sdk":
		if claudeSDKFactory == nil {
			return nil, fmt.Errorf("claude_sdk runtime not registered (import _ \"github.com/openai/symphony/go/internal/agent/claudesdk\")")
		}
		return claudeSDKFactory(cfg)
	default:
		return nil, fmt.Errorf("unsupported agent.runtime: %s", cfg.Agent.Runtime)
	}
}
