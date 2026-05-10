package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/prompt"
	"github.com/openai/symphony/go/internal/transcript"
)

// dispatchOne runs the full lifecycle for a single issue inside its own
// goroutine: workspace ensure → hooks → session → turn loop → cleanup.
// When agent.pipeline is configured, dispatch is delegated to the
// pipeline-aware path instead.
func (o *Orchestrator) dispatchOne(ctx context.Context, issue domain.Issue) {
	if len(o.cfg.Agent.Pipeline) > 0 {
		o.dispatchPipeline(ctx, issue)
		return
	}
	log := o.log.WithIssue(issue)
	log.Info("dispatch started")

	// Open a per-dispatch transcript writer. Failure to open is logged
	// and the dispatch continues — the audit trail is best-effort.
	var tw *transcript.Writer
	if path := o.transcriptPath(issue); path != "" {
		var err error
		tw, err = transcript.NewWriter(path)
		if err != nil {
			log.Warn("transcript writer init failed", "err", err.Error())
			tw = nil
		}
	}

	defer func() {
		if tw != nil {
			_ = tw.Close() //nolint:errcheck
		}
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

	o.mu.Lock()
	if entry, ok := o.running[issue.ID]; ok {
		entry.workspacePath = ws.Path
	}
	o.mu.Unlock()

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

	o.mu.Lock()
	if entry, ok := o.running[issue.ID]; ok {
		entry.sessionID = sess.ID
	}
	o.mu.Unlock()
	o.notify()

	sessionLog := log.WithSession(sess.ID)
	runErr := o.runTurnLoop(ctx, issue, ws, sess, sessionLog, tw)

	// StopSession — always called; errors are logged and ignored.
	if stopErr := o.runtime.StopSession(ctx, sess); stopErr != nil {
		log.Warn("stop session error (ignored)", "err", fmt.Sprintf("%v", stopErr))
	}

	// after_run hook — fires only when the run was not cancelled (T19).
	// Cancellation is detected either via the per-issue context being
	// done while runState is cancel_requested, or via the operator-level
	// cancelRequested check. Skipping avoids running long PR-creation
	// or notify scripts against partial state.
	cancelled := o.cancelRequested(issue.ID) || (ctx.Err() == context.Canceled && o.cancelRequested(issue.ID))
	if o.cfg.Hooks.AfterRun != "" {
		if cancelled {
			log.Info("after_run skipped: run cancelled")
		} else {
			timeout := time.Duration(o.cfg.Hooks.TimeoutMs) * time.Millisecond
			result, hookErr := o.ws.RunHook(context.Background(), ws, o.cfg.Hooks.AfterRun, timeout)
			if hookErr != nil || result.ExitCode != 0 || result.TimedOut {
				log.Warn("after_run hook failed (ignored)",
					"exit_code", result.ExitCode,
					"timed_out", result.TimedOut,
				)
			}
		}
	}

	if runErr != nil && ctx.Err() == nil && !o.cancelRequested(issue.ID) {
		o.scheduleRetry(issue, runErr)
	}

	// T20: defence-in-depth — if RequestCancel arrived too late to clean
	// up (race against the dispatch loop returning), do the cleanup here
	// before releaseClaim drops the entry. RequestCancel itself already
	// cleans up synchronously; this only fires when the dispatch path
	// detected ctx cancellation but the cancelRequested check came after
	// the operator's RequestCancel call.
	if cancelled {
		o.cleanupCancelledWorkspace(ws.Path)
	}
}

// cancelRequested reports whether the running entry for issueID was
// explicitly cancelled via RequestCancel. Used by dispatchOne to suppress
// the retry-on-error path for cancelled dispatches.
func (o *Orchestrator) cancelRequested(issueID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.running[issueID]
	if !ok {
		return false
	}
	return entry.state == runStateCancelRequested
}

// observePauseOrCancel inspects the run entry's state at the top of each
// turn. Returns true when the loop should exit (cancellation or context
// already done). Blocks while the entry is paused.
func (o *Orchestrator) observePauseOrCancel(ctx context.Context, issueID string, log *observability.Logger, turn int) bool {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok {
		o.mu.Unlock()
		return false
	}
	switch entry.state {
	case runStateCancelRequested:
		o.mu.Unlock()
		log.Info("turn loop cancelled by operator", "turn", turn)
		return true
	case runStatePauseRequested:
		// Promote pause_requested → paused now that we've reached a quiescent
		// point between turns.
		entry.state = runStatePaused
		ch := entry.pauseCh
		o.mu.Unlock()
		o.notify()
		if ch != nil {
			log.Info("turn loop paused (waiting for resume)", "turn", turn)
			select {
			case <-ch:
			case <-ctx.Done():
				return true
			}
			log.Info("turn loop resumed", "turn", turn)
		}
		return o.cancelRequested(issueID)
	default:
		o.mu.Unlock()
		return false
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
	tw *transcript.Writer,
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

		// Honour operator-requested pause / cancel.
		if cancelled := o.observePauseOrCancel(ctx, issue.ID, log, turn); cancelled {
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

		prEmitter := agent.NewPRLinkEmitter(func(ev agent.Event) {
			if pl, ok := ev.Payload.(agent.PRLinkPayload); ok {
				o.recordPRLink(issue, pl)
			}
		})
		currentTurn := turn
		cb := func(ev agent.Event) {
			log.Debug("agent event", "kind", string(ev.Kind))
			if ev.Kind == agent.EventAssistantMessage || ev.Kind == agent.EventToolCall || ev.Kind == agent.EventToolResult || ev.Kind == agent.EventOtherMessage {
				prEmitter.Scan(sess.ID, fmt.Sprintf("%v", ev.Payload))
			}
			tev := transcript.Event{
				Ts:              ev.Timestamp,
				IssueID:         issue.ID,
				IssueIdentifier: issue.Identifier,
				SessionID:       sess.ID,
				Turn:            currentTurn,
				Kind:            string(ev.Kind),
				Payload:         ev.Payload,
			}
			if tw != nil {
				_ = tw.Append(tev) //nolint:errcheck
			}
			if o.transcriptBus != nil {
				o.transcriptBus.Publish(tev)
			}
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

		o.recordTurnComplete(issue.ID, turn, result)
		o.notify()

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

// recordTurnComplete folds turn-level token usage and turn count into the
// running entry and the global agent_totals so the observability snapshot
// reflects post-turn state. Mirrors Elixir's apply_codex_token_delta /
// integrate_codex_update path that runs before notify_dashboard().
func (o *Orchestrator) recordTurnComplete(issueID string, turn int, result agent.TurnResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	entry, ok := o.running[issueID]
	if !ok {
		return
	}

	entry.turnCount = turn
	entry.lastEvent = "turn_completed"
	entry.lastEventAt = time.Now().UTC()
	entry.tokens.InputTokens += result.Tokens.InputTokens
	entry.tokens.OutputTokens += result.Tokens.OutputTokens
	entry.tokens.TotalTokens += result.Tokens.TotalTokens

	o.agentTotals.InputTokens += result.Tokens.InputTokens
	o.agentTotals.OutputTokens += result.Tokens.OutputTokens
	o.agentTotals.TotalTokens += result.Tokens.TotalTokens
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

// transcriptPath returns the absolute path to the per-issue transcript
// JSONL file. Transcripts live under <workspace_root>/.transcripts/<key>.jsonl
// — sibling to per-issue workspace dirs, not inside them — so RemoveForIssue
// (which RemoveAlls <workspace_root>/<key>) does not delete the audit trail.
// Returns "" when the workspace root is unset (test orchestrators).
func (o *Orchestrator) transcriptPath(issue domain.Issue) string {
	root := o.cfg.Workspace.Root
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".transcripts", issue.WorkspaceKey()+".jsonl")
}

// TranscriptPathForIdentifier returns the transcript file path for the
// given issue identifier, suitable for the GET /api/v1/issues/{id}/transcript
// handler.
func (o *Orchestrator) TranscriptPathForIdentifier(identifier string) string {
	root := o.cfg.Workspace.Root
	if root == "" {
		return ""
	}
	key := domain.Issue{Identifier: identifier}.WorkspaceKey()
	return filepath.Join(root, ".transcripts", key+".jsonl")
}
