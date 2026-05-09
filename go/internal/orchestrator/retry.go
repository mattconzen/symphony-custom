package orchestrator

import (
	"fmt"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

const (
	// defaultRetryBaseMs is the base backoff in milliseconds for attempt 1.
	defaultRetryBaseMs = 5000
	// maxRetryAttempts caps the number of automatic retries per issue.
	maxRetryAttempts = 10
)

// computeBackoffMs returns the exponential backoff duration in milliseconds for
// the given attempt number per SPEC §8.4:
//
//	delay = min(baseMs * 2^(attempt-1), maxBackoffMs)
//
// attempt is 1-indexed (attempt=1 → baseMs, attempt=2 → 2*baseMs, …).
func computeBackoffMs(attempt int, cfg config.Config) int64 {
	base := int64(defaultRetryBaseMs)
	max := int64(cfg.Agent.MaxRetryBackoffMs)
	if max <= 0 {
		max = 300_000
	}

	// 2^(attempt-1); saturate at 63 to avoid int64 overflow.
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 62 {
		shift = 62
	}
	delay := base << shift
	if delay > max || delay < 0 { // overflow guard
		delay = max
	}
	return delay
}

// scheduleRetry schedules a retry for the issue after an exponential backoff
// delay per SPEC §8.4. The issue is re-added to the tracker's active states by
// calling the tracker's UpdateIssueState after the delay expires.
//
// The function records a RetryEntry on the orchestrator and fires an
// AfterFunc timer to trigger re-dispatch.
//
// IMPORTANT: This method uses o.rootCtx (the orchestrator-level context set in
// Run) rather than the per-issue dispatch context. The per-issue context is
// already cancelled by the time the retry timer fires, so using it would
// silently drop all retries.
func (o *Orchestrator) scheduleRetry(issue domain.Issue, runErr error) {
	o.mu.Lock()
	entry, exists := o.retryAttempts[issue.ID]
	if !exists {
		entry = &domain.RetryEntry{
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Attempt:    0,
			Error:      "",
		}
		o.retryAttempts[issue.ID] = entry
	}
	entry.Attempt++
	attempt := entry.Attempt
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	entry.Error = errMsg

	// Cap retries.
	if attempt > maxRetryAttempts {
		o.log.Warn("max retries exceeded, not scheduling retry",
			"issue_id", issue.ID,
			"attempt", attempt,
		)
		delete(o.retryAttempts, issue.ID)
		o.mu.Unlock()
		return
	}

	delayMs := computeBackoffMs(attempt, o.cfg)
	entry.DueAtMs = time.Now().UnixMilli() + delayMs
	// Capture rootCtx under the lock to avoid a race with Run() setting it.
	rootCtx := o.rootCtx
	o.mu.Unlock()

	o.log.Info("scheduling retry",
		"issue_id", issue.ID,
		"attempt", attempt,
		"delay_ms", delayMs,
		"err", errMsg,
	)

	// Mirrors Elixir handle_info({:retry_issue, ...}) → notify_dashboard().
	o.notify()

	time.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
		// Use the orchestrator-level context (not the per-issue dispatch context,
		// which is already cancelled by the time this timer fires).
		if rootCtx == nil || rootCtx.Err() != nil {
			return
		}
		// Re-activate the issue in the tracker so the next poll picks it up.
		if len(o.cfg.Tracker.ActiveStates) > 0 {
			state := o.cfg.Tracker.ActiveStates[0]
			if err := o.tracker.UpdateIssueState(rootCtx, issue.ID, state); err != nil {
				o.log.Warn("retry: failed to reactivate issue",
					"issue_id", issue.ID,
					"err", fmt.Sprintf("%v", err),
				)
			}
		}
		// Release from retryAttempts bookkeeping so it can be re-dispatched.
		o.mu.Lock()
		delete(o.retryAttempts, issue.ID)
		o.mu.Unlock()
		o.notify()
	})
}
