package orchestrator_test

// TestRetryUsesRootContext is a regression test for the bug where scheduleRetry
// was passed the per-issue dispatch context (runCtx), which was already cancelled
// by the time the retry timer fired. This caused all retries to silently drop.
//
// Fix: scheduleRetry now uses o.rootCtx (stored in Run()) instead of the
// per-issue context.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

// failOnFirstTurnRuntime fails on the first StartSession call, then succeeds.
// This triggers scheduleRetry on the first dispatch and verifies the retry
// actually re-dispatches the issue (i.e., rootCtx was used, not runCtx).
type failOnFirstRuntime struct {
	callCount int
	succeeded chan struct{}
}

func (r *failOnFirstRuntime) StartSession(_ context.Context, _ domain.Workspace) (agent.Session, error) {
	r.callCount++
	if r.callCount == 1 {
		return agent.Session{}, errors.New("injected startup failure for retry regression test")
	}
	return agent.Session{ID: "retry-session"}, nil
}

func (r *failOnFirstRuntime) StopSession(_ context.Context, _ agent.Session) error { return nil }

func (r *failOnFirstRuntime) RunTurn(_ context.Context, sess agent.Session, _ string, _ domain.Issue, _ agent.EventCallback) (agent.TurnResult, error) {
	select {
	case r.succeeded <- struct{}{}:
	default:
	}
	return agent.TurnResult{Status: agent.TurnCompleted, SessionID: sess.ID}, nil
}

func TestRetryUsesRootContext(t *testing.T) {
	cfg := buildCfg(t)
	// Use a very short retry backoff so the test runs fast.
	// The default is 5000ms which is too slow. We'll override by using a very
	// small MaxRetryBackoffMs so computeBackoffMs returns 1000ms (1s).
	cfg.Agent.MaxRetryBackoffMs = 1000

	issue := domain.Issue{
		ID:         "retry-regression",
		Identifier: "RETRY-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})

	rt := &failOnFirstRuntime{succeeded: make(chan struct{}, 1)}

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	// Wait for the second StartSession call (after the retry timer fires).
	// Allow up to 15 seconds for the retry to fire (backoff = 1000ms with
	// defaultRetryBaseMs=5000ms capped at MaxRetryBackoffMs=1000ms).
	select {
	case <-rt.succeeded:
		// Retry fired and succeeded — root context was used correctly.
	case <-time.After(15 * time.Second):
		t.Fatal("retry never fired; regression: scheduleRetry may still be using per-issue context")
	}

	cancel()
	<-done
}
