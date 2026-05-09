package mock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/domain"
)

func TestMock_ScriptedTurnStatus(t *testing.T) {
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
		},
	})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	result, err := rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, result.Status)
	assert.Equal(t, 10, result.Tokens.InputTokens)
	assert.Equal(t, 5, result.Tokens.OutputTokens)
	assert.Equal(t, 15, result.Tokens.TotalTokens)
}

func TestMock_ScriptedTurnError(t *testing.T) {
	sentinelErr := errors.New("agent exploded")
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{Status: agent.TurnFailed, Err: sentinelErr},
		},
	})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	result, err := rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, nil)
	require.ErrorIs(t, err, sentinelErr)
	assert.Equal(t, agent.TurnFailed, result.Status)
}

func TestMock_RecordPrompts(t *testing.T) {
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
		},
	})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	issue := domain.Issue{ID: "42", Identifier: "ISSUE-42", Title: "My issue"}
	_, err = rt.RunTurn(ctx, sess, "prompt one", issue, nil)
	require.NoError(t, err)
	_, err = rt.RunTurn(ctx, sess, "prompt two", issue, nil)
	require.NoError(t, err)

	// Access via interface method
	type recorder interface {
		RecordedPrompts() []mock.RecordedTurn
		RecordedTurnCount() int
	}
	rec, ok := rt.(recorder)
	require.True(t, ok, "mock runtime should implement RecordedPrompts/RecordedTurnCount")

	prompts := rec.RecordedPrompts()
	require.Len(t, prompts, 2)
	assert.Equal(t, "prompt one", prompts[0].Prompt)
	assert.Equal(t, "ISSUE-42", prompts[0].Issue.Identifier)
	assert.Equal(t, "prompt two", prompts[1].Prompt)
	assert.Equal(t, 2, rec.RecordedTurnCount())
}

func TestMock_EventsForwarded(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventAssistantMessage, Timestamp: time.Now(), SessionID: "s1", Payload: "hello"},
		{Kind: agent.EventTurnCompleted, Timestamp: time.Now(), SessionID: "s1"},
	}

	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted, Events: events},
		},
	})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	var received []agent.Event
	cb := func(ev agent.Event) { received = append(received, ev) }

	_, err = rt.RunTurn(ctx, sess, "hello", domain.Issue{ID: "1"}, cb)
	require.NoError(t, err)
	require.Len(t, received, 2)
	assert.Equal(t, agent.EventAssistantMessage, received[0].Kind)
	assert.Equal(t, agent.EventTurnCompleted, received[1].Kind)
}

func TestMock_DoubleStopSessionIsNoOp(t *testing.T) {
	rt := mock.New(mock.MockOpts{})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	require.NoError(t, rt.StopSession(ctx, sess))
	require.NoError(t, rt.StopSession(ctx, sess)) // second call must not error
}

func TestMock_StartSessionNonEmptyID(t *testing.T) {
	rt := mock.New(mock.MockOpts{})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
}

func TestMock_ExhaustedScriptsReuseLastTurn(t *testing.T) {
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{TotalTokens: 99}},
		},
	})

	ctx := context.Background()
	ws := domain.Workspace{Path: "/tmp/ws", Key: "ws"}
	sess, err := rt.StartSession(ctx, ws)
	require.NoError(t, err)

	// Call RunTurn more than the number of scripts
	for i := 0; i < 5; i++ {
		result, err := rt.RunTurn(ctx, sess, "p", domain.Issue{ID: "1"}, nil)
		require.NoError(t, err)
		assert.Equal(t, agent.TurnCompleted, result.Status)
		assert.Equal(t, 99, result.Tokens.TotalTokens)
	}
}
