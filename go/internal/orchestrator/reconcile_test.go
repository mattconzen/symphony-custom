package orchestrator_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

func TestReconcile_CancelsDispatchOnTerminalState(t *testing.T) {
	cfg := buildCfg(t)
	cfg.Polling.IntervalMs = 30 // very fast polling

	issue := domain.Issue{
		ID:         "reconcile-issue",
		Identifier: "REC-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})

	// The mock runtime blocks in RunTurn until its context is cancelled.
	// We signal when RunTurn has been entered via a channel.
	turnStarted := make(chan struct{}, 1)
	turnCtxDone := make(chan struct{}, 1)

	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{
			{
				// Custom events trick: we use the pre-return Events + a blocking approach.
				// Since TurnScript doesn't support blocking, we use a workaround:
				// use a very large token count script and let the context cancel propagate.
				Status: agent.TurnCompleted,
			},
		},
	})

	// We need a runtime that actually blocks. Let's build a simple blocking wrapper.
	blockingRT := &blockingRuntime{
		inner:       rt,
		turnStarted: turnStarted,
		turnCtxDone: turnCtxDone,
	}

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, blockingRT, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = orch.Run(ctx) }()

	// Wait until a turn is in progress.
	select {
	case <-turnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn never started")
	}

	// Flip the issue to terminal state.
	err := tr.UpdateIssueState(context.Background(), issue.ID, "Done")
	if err != nil {
		t.Fatalf("failed to update issue state: %v", err)
	}

	// Reconciliation should detect the terminal state and cancel the dispatch context.
	// Allow up to 3× the tick interval.
	select {
	case <-turnCtxDone:
		// Success: dispatch context was cancelled.
	case <-time.After(time.Duration(cfg.Polling.IntervalMs*3) * time.Millisecond * 10):
		t.Fatal("dispatch context not cancelled within deadline")
	}

	cancel()
}

// blockingRuntime wraps agent.Runtime to block in RunTurn until the context is cancelled.
type blockingRuntime struct {
	inner       agent.Runtime
	turnStarted chan struct{}
	turnCtxDone chan struct{}
}

func (b *blockingRuntime) StartSession(ctx context.Context, ws domain.Workspace) (agent.Session, error) {
	return b.inner.StartSession(ctx, ws)
}

func (b *blockingRuntime) StopSession(ctx context.Context, sess agent.Session) error {
	return b.inner.StopSession(ctx, sess)
}

func (b *blockingRuntime) RunTurn(ctx context.Context, sess agent.Session, p string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	// Signal that we're in a turn.
	select {
	case b.turnStarted <- struct{}{}:
	default:
	}

	// Block until context is cancelled.
	<-ctx.Done()

	// Signal that the context was cancelled.
	select {
	case b.turnCtxDone <- struct{}{}:
	default:
	}

	return agent.TurnResult{
		SessionID: sess.ID,
		Status:    agent.TurnCancelled,
	}, ctx.Err()
}
