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

func init() {
	RegisterMockFactory = func(fn func() Runtime) {
		mockFactory = fn
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
)

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
		return nil, fmt.Errorf("unsupported agent.runtime: codex (will be added in a later phase)")
	case "claude":
		return nil, fmt.Errorf("unsupported agent.runtime: claude (will be added in a later phase)")
	default:
		return nil, fmt.Errorf("unsupported agent.runtime: %s", cfg.Agent.Runtime)
	}
}
