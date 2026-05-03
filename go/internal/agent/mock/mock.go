// Package mock provides a scriptable mock implementation of agent.Runtime for
// testing per SPEC Phase 1.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/domain"
)

// TurnScript describes the outcome of one scripted turn.
type TurnScript struct {
	Status agent.TurnStatus
	Tokens agent.TokenUsage
	Err    error
	// Events are forwarded to the EventCallback before RunTurn returns.
	Events []agent.Event
}

// RecordedTurn captures a single RunTurn invocation when RecordPrompts is true.
type RecordedTurn struct {
	Prompt string
	Issue  domain.Issue
}

// MockOpts configures the mock runtime.
type MockOpts struct {
	// Turns is a sequence of scripted outcomes. If exhausted, the last script is
	// reused. If Turns is empty, RunTurn returns TurnCompleted with zero tokens.
	Turns []TurnScript
	// RecordPrompts, when true, appends every (prompt, issue) pair to an
	// internal log accessible via RecordedPrompts().
	RecordPrompts bool
}

// mockRuntime implements agent.Runtime using scripted responses.
type mockRuntime struct {
	opts MockOpts

	mu        sync.Mutex
	turnIndex int
	sessions  map[string]bool // tracks open sessions
	recorded  []RecordedTurn
	turnCount int
}

// New returns a new agent.Runtime that executes scripted turns.
func New(opts MockOpts) agent.Runtime {
	return &mockRuntime{
		opts:     opts,
		sessions: make(map[string]bool),
	}
}

// StartSession creates a new session with a unique ID.
func (m *mockRuntime) StartSession(_ context.Context, ws domain.Workspace) (agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("mock-session-%d", time.Now().UnixNano())
	m.sessions[id] = true
	return agent.Session{
		ID:   id,
		Impl: nil,
	}, nil
}

// StopSession marks the session as stopped. Double-calling is a no-op.
func (m *mockRuntime) StopSession(_ context.Context, sess agent.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sess.ID)
	return nil
}

// RunTurn executes the next scripted turn. If the Turns slice is exhausted, the
// last entry is reused. If Turns is empty, TurnCompleted is returned.
func (m *mockRuntime) RunTurn(ctx context.Context, sess agent.Session, prompt string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	m.mu.Lock()
	script := m.currentScript()
	if m.opts.RecordPrompts {
		m.recorded = append(m.recorded, RecordedTurn{Prompt: prompt, Issue: issue})
	}
	m.turnCount++
	m.advanceScript()
	m.mu.Unlock()

	// Emit scripted events (outside the lock so callbacks can freely use the mock)
	if cb != nil {
		for _, ev := range script.Events {
			if ctx.Err() != nil {
				break
			}
			cb(ev)
		}
	}

	if ctx.Err() != nil {
		return agent.TurnResult{
			SessionID: sess.ID,
			Status:    agent.TurnCancelled,
		}, ctx.Err()
	}

	if script.Err != nil {
		return agent.TurnResult{
			SessionID: sess.ID,
			Status:    agent.TurnFailed,
			Tokens:    script.Tokens,
			Err:       script.Err,
		}, script.Err
	}

	return agent.TurnResult{
		SessionID: sess.ID,
		Status:    script.Status,
		Tokens:    script.Tokens,
	}, nil
}

// RecordedPrompts returns the list of recorded turns (only populated when
// MockOpts.RecordPrompts == true).
func (m *mockRuntime) RecordedPrompts() []RecordedTurn {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedTurn, len(m.recorded))
	copy(out, m.recorded)
	return out
}

// RecordedTurnCount returns the total number of RunTurn calls made.
func (m *mockRuntime) RecordedTurnCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnCount
}

// currentScript returns the current TurnScript without modifying state.
// Caller must hold m.mu.
func (m *mockRuntime) currentScript() TurnScript {
	if len(m.opts.Turns) == 0 {
		return TurnScript{Status: agent.TurnCompleted}
	}
	if m.turnIndex >= len(m.opts.Turns) {
		return m.opts.Turns[len(m.opts.Turns)-1]
	}
	return m.opts.Turns[m.turnIndex]
}

// advanceScript increments the turn index (stopping at the last entry).
// Caller must hold m.mu.
func (m *mockRuntime) advanceScript() {
	if len(m.opts.Turns) > 0 && m.turnIndex < len(m.opts.Turns)-1 {
		m.turnIndex++
	}
}

// init registers the mock factory with the agent package so that
// agent.New(cfg) works when cfg.Agent.Runtime == "mock" and this package
// is imported (e.g. via a blank import in main or tests).
func init() {
	if agent.RegisterMockFactory != nil {
		agent.RegisterMockFactory(func() agent.Runtime {
			return New(MockOpts{})
		})
	}
}
