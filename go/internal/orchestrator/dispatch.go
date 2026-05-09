package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/prompt"
)

// dispatchOne runs the full lifecycle for a single issue inside its own
// goroutine: workspace ensure → hooks → session → turn loop → cleanup.
func (o *Orchestrator) dispatchOne(ctx context.Context, issue domain.Issue) {
	log := o.log.WithIssue(issue)
	log.Info("dispatch started")

	defer func() {
		o.releaseClaim(issue.ID)
		log.Info("dispatch finished")
	}()

	// Ensure workspace (runs after_create hook internally when CreatedNow is true).
	ws, err := o.ws.EnsureForIssue(ctx, issue)
	if err != nil {
		log.Error("workspace ensure failed", "err", fmt.Sprintf("%v", err))
		o.scheduleRetry(issue, err)
		return
	}

	// before_run hook.
	if o.cfg.Hooks.BeforeRun != "" {
		timeout := time.Duration(o.cfg.Hooks.TimeoutMs) * time.Millisecond
		result, hookErr := o.ws.RunHook(ctx, ws, o.cfg.Hooks.BeforeRun, timeout)
		if hookErr != nil || result.ExitCode != 0 || result.TimedOut {
			log.Warn("before_run hook failed",
				"exit_code", result.ExitCode,
				"timed_out", result.TimedOut,
			)
		}
	}

	// Start agent session.
	sess, err := o.runtime.StartSession(ctx, ws)
	if err != nil {
		log.Error("start session failed", "err", fmt.Sprintf("%v", err))
		o.scheduleRetry(issue, err)
		return
	}

	sessionLog := log.WithSession(sess.ID)
	runErr := o.runTurnLoop(ctx, issue, ws, sess, sessionLog)

	// StopSession — always called; errors are logged and ignored.
	if stopErr := o.runtime.StopSession(ctx, sess); stopErr != nil {
		log.Warn("stop session error (ignored)", "err", fmt.Sprintf("%v", stopErr))
	}

	// after_run hook — always fires; failures logged-and-ignored per SPEC §9.4.
	if o.cfg.Hooks.AfterRun != "" {
		timeout := time.Duration(o.cfg.Hooks.TimeoutMs) * time.Millisecond
		result, hookErr := o.ws.RunHook(context.Background(), ws, o.cfg.Hooks.AfterRun, timeout)
		if hookErr != nil || result.ExitCode != 0 || result.TimedOut {
			log.Warn("after_run hook failed (ignored)",
				"exit_code", result.ExitCode,
				"timed_out", result.TimedOut,
			)
		}
	}

	if runErr != nil && ctx.Err() == nil {
		o.scheduleRetry(issue, runErr)
	}
}

// runTurnLoop executes turns until max_turns is reached, the issue reaches a
// terminal state, or the context is cancelled.
func (o *Orchestrator) runTurnLoop(
	ctx context.Context,
	issue domain.Issue,
	ws domain.Workspace,
	sess agent.Session,
	log *observability.Logger,
) error {
	maxTurns := o.cfg.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}

	terminalSet := make(map[string]bool, len(o.cfg.Tracker.TerminalStates))
	for _, s := range o.cfg.Tracker.TerminalStates {
		terminalSet[strings.ToLower(s)] = true
	}

	// feedback holds between_turns hook output to inject into the next turn's prompt.
	// It is local to this goroutine — no lock required.
	var feedback string

	for turn := 1; turn <= maxTurns; turn++ {
		// Check context cancellation before each turn.
		if ctx.Err() != nil {
			log.Info("turn loop cancelled", "turn", turn)
			return ctx.Err()
		}

		// Refresh issue state to detect terminal transitions (reconciliation).
		states, fetchErr := o.tracker.FetchIssueStatesByIDs(ctx, []string{issue.ID})
		if fetchErr == nil && len(states) > 0 {
			if terminalSet[strings.ToLower(states[0].State)] {
				log.Info("issue reached terminal state, stopping turns",
					"state", states[0].State,
					"turn", turn,
				)
				return nil
			}
			issue = states[0]
		}

		// Build the prompt for this turn.
		var p string
		var buildErr error
		if turn == 1 {
			p, buildErr = buildFirstTurnPrompt(issue, o)
			if buildErr != nil {
				log.Error("failed to build first-turn prompt", "err", fmt.Sprintf("%v", buildErr))
				return buildErr
			}
		} else {
			p = buildContinuationPrompt(turn, maxTurns, feedback)
		}

		log.Info("running turn", "turn", turn)

		cb := func(ev agent.Event) {
			log.Debug("agent event", "kind", string(ev.Kind))
		}

		result, turnErr := o.runtime.RunTurn(ctx, sess, p, issue, cb)
		if turnErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Error("turn failed", "turn", turn, "err", fmt.Sprintf("%v", turnErr))
			return turnErr
		}

		log.Info("turn completed",
			"turn", turn,
			"status", fmt.Sprintf("%d", result.Status),
			"tokens_total", result.Tokens.TotalTokens,
		)

		switch result.Status {
		case agent.TurnCompleted:
			// Issue is still active — run between_turns hook before continuing.
			feedback = o.runBetweenTurnsHook(ctx, ws, log, turn)
		case agent.TurnCancelled:
			return ctx.Err()
		case agent.TurnFailed:
			return fmt.Errorf("turn %d failed: %w", turn, result.Err)
		}
	}

	log.Warn("max turns reached", "max_turns", maxTurns)
	return nil
}

// buildFirstTurnPrompt renders the workflow template for the first turn.
func buildFirstTurnPrompt(issue domain.Issue, o *Orchestrator) (string, error) {
	if o.promptTemplate == "" {
		return fmt.Sprintf("You are working on issue %s.", issue.Identifier), nil
	}
	return prompt.Render(o.promptTemplate, prompt.Vars{Issue: issue})
}

// buildContinuationPrompt builds the prompt for turns > 1 per SPEC §12.3.
// If feedback is non-empty, appends a "Validation feedback" section.
func buildContinuationPrompt(turnNumber, maxTurns int, feedback string) string {
	s := fmt.Sprintf(`Continuation guidance:

- The previous agent turn completed normally, but the issue is still in an active state.
- This is continuation turn #%d of %d.
- Resume from the current workspace state instead of restarting from scratch.`, turnNumber, maxTurns)

	if feedback != "" {
		s += fmt.Sprintf(`

Validation feedback (between_turns hook, last turn):

`+"```"+`
%s
`+"```"+`

Address these findings before continuing the original task.`, feedback)
	}
	return s
}

// runBetweenTurnsHook runs the between_turns hook (if configured) after a
// completed turn. It returns the feedback string to inject into the next turn's
// continuation prompt. It never aborts the run — failures are logged-and-ignored
// per SPEC §5.3.4.
func (o *Orchestrator) runBetweenTurnsHook(
	ctx context.Context,
	ws domain.Workspace,
	log *observability.Logger,
	turn int,
) string {
	if o.cfg.Hooks.BetweenTurns == "" {
		return ""
	}

	timeout := time.Duration(o.cfg.Hooks.TimeoutMs) * time.Millisecond
	result, hookErr := o.ws.RunHook(ctx, ws, o.cfg.Hooks.BetweenTurns, timeout)

	if hookErr != nil {
		log.Warn("between_turns hook error (ignored)", "turn", turn, "err", fmt.Sprintf("%v", hookErr))
		return ""
	}

	if result.TimedOut {
		log.Warn("between_turns hook timed out (ignored)", "turn", turn, "timeout_ms", o.cfg.Hooks.TimeoutMs)
		stdout := truncateOutput(result.Stdout, 4*1024-100)
		return fmt.Sprintf("<hook timed out after %dms>\n%s", o.cfg.Hooks.TimeoutMs, stdout)
	}

	if result.ExitCode != 0 {
		log.Warn("between_turns hook failed (non-zero exit, feedback captured)",
			"turn", turn,
			"exit_code", result.ExitCode,
		)
		return truncateOutput(result.Stdout, 4*1024)
	}

	log.Info("between_turns hook succeeded", "turn", turn)
	return ""
}

// truncateOutput truncates output to at most maxBytes. If truncated, appends a
// marker line.
func truncateOutput(out []byte, maxBytes int) string {
	if len(out) <= maxBytes {
		return string(out)
	}
	truncated := string(out[:maxBytes])
	return truncated + "\n... [truncated, full output suppressed]\n"
}
