package orchestrator

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/openai/symphony/go/internal/domain"
)

// durableState is the on-disk projection of the orchestrator-owned state.
// Only fields that survive a process restart belong here; in-flight
// goroutine state (workspaces, agent subprocesses) is rebuilt on next
// dispatch.
type durableState struct {
	RetryAttempts map[string]*domain.RetryEntry  `json:"retry_attempts"`
	PullRequests  map[string]domain.PullRequest  `json:"pull_requests"`
	AgentTotals   agentTotalsDurable             `json:"agent_totals"`
	KanbanIssues  []domain.Issue                 `json:"kanban_issues,omitempty"`
	KanbanSpecs   map[string]bool                `json:"kanban_specs,omitempty"`
}

// agentTotalsDurable mirrors observability.TokenTotals. We declare it
// locally so the durable package does not pull observability into its
// import graph (and so we never silently couple wire format to the
// observability struct).
type agentTotalsDurable struct {
	TotalTokens    int `json:"total_tokens"`
	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
	SecondsRunning int `json:"seconds_running"`
}

// loadDurable repopulates state from disk. Missing files are not an
// error — they just mean this is a fresh install. Other failures are
// returned so the caller can refuse to start with corrupt state.
func (o *Orchestrator) loadDurable() error {
	if o.durable == nil {
		return nil
	}
	var state durableState
	err := o.durable.Load("orchestrator", &state)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if state.RetryAttempts != nil {
		o.retryAttempts = state.RetryAttempts
	}
	if state.PullRequests != nil {
		o.pullRequests = state.PullRequests
	}
	o.agentTotals.TotalTokens = state.AgentTotals.TotalTokens
	o.agentTotals.InputTokens = state.AgentTotals.InputTokens
	o.agentTotals.OutputTokens = state.AgentTotals.OutputTokens
	o.agentTotals.SecondsRunning = state.AgentTotals.SecondsRunning
	if state.KanbanIssues != nil {
		o.kanbanIssues = state.KanbanIssues
	}
	if state.KanbanSpecs != nil {
		o.kanbanSpecs = state.KanbanSpecs
	}
	return nil
}

// durablePersisterState tracks the rate-limit metadata for save-failure
// warnings so a stuck disk doesn't spam the log every 200ms. Fields are
// only touched by runDurablePersister / saveDurable on the same goroutine,
// so no separate lock is required.
type durablePersisterState struct {
	lastErrLogAt time.Time
	lastErrStr   string
	hadError     bool
}

// durableErrLogInterval is the minimum wall-clock gap between two
// identical-error warn logs. New error strings always log immediately.
const durableErrLogInterval = 30 * time.Second

// saveDurable snapshots state and writes it under "orchestrator". Best-
// effort: errors are logged and the run continues. The dirty flag is
// cleared only on success so a transient write failure retries on the
// next debounce tick. The optional ps argument provides log rate-limit
// state; pass nil to log every failure (used by the one-shot final save).
func (o *Orchestrator) saveDurable() {
	o.saveDurableWithState(nil)
}

func (o *Orchestrator) saveDurableWithState(ps *durablePersisterState) {
	if o.durable == nil {
		return
	}
	o.mu.Lock()
	state := durableState{
		RetryAttempts: cloneRetryMap(o.retryAttempts),
		PullRequests:  clonePRMap(o.pullRequests),
		AgentTotals: agentTotalsDurable{
			TotalTokens:    o.agentTotals.TotalTokens,
			InputTokens:    o.agentTotals.InputTokens,
			OutputTokens:   o.agentTotals.OutputTokens,
			SecondsRunning: o.agentTotals.SecondsRunning,
		},
		KanbanIssues: append([]domain.Issue(nil), o.kanbanIssues...),
		KanbanSpecs:  cloneSpecsMap(o.kanbanSpecs),
	}
	o.mu.Unlock()

	if err := o.durable.Save("orchestrator", state); err != nil {
		o.maybeLogSaveErr(ps, err)
		return
	}
	if ps != nil && ps.hadError {
		o.log.Info("durable: orchestrator saves recovered")
		ps.hadError = false
		ps.lastErrStr = ""
		ps.lastErrLogAt = time.Time{}
	}
	o.mu.Lock()
	o.dirty = false
	o.mu.Unlock()
}

// maybeLogSaveErr emits a warn log for the given save error, rate-limited
// per durableErrLogInterval and gated on error-string change so a stuck
// disk doesn't flood logs every 200ms tick. When ps is nil the error is
// always logged (used by callers that don't maintain rate-limit state).
func (o *Orchestrator) maybeLogSaveErr(ps *durablePersisterState, err error) {
	msg := err.Error()
	if ps == nil {
		o.log.Warn("durable: orchestrator save failed", "err", msg)
		return
	}
	now := time.Now()
	changed := msg != ps.lastErrStr
	stale := ps.lastErrLogAt.IsZero() || now.Sub(ps.lastErrLogAt) >= durableErrLogInterval
	if changed || stale {
		o.log.Warn("durable: orchestrator save failed", "err", msg)
		ps.lastErrLogAt = now
		ps.lastErrStr = msg
	}
	ps.hadError = true
}

// runDurablePersister is a background goroutine that watches the dirty
// flag and writes state with a 200ms debounce. It returns when ctx is
// cancelled, performing one final synchronous save on the way out.
func (o *Orchestrator) runDurablePersister(ctx context.Context) {
	if o.durable == nil {
		return
	}
	const debounce = 200 * time.Millisecond
	ticker := time.NewTicker(debounce)
	defer ticker.Stop()

	ps := &durablePersisterState{}

	for {
		select {
		case <-ctx.Done():
			o.saveDurableWithState(ps)
			return
		case <-ticker.C:
			o.mu.Lock()
			dirty := o.dirty
			o.mu.Unlock()
			if dirty {
				o.saveDurableWithState(ps)
			}
		}
	}
}

func cloneRetryMap(in map[string]*domain.RetryEntry) map[string]*domain.RetryEntry {
	if in == nil {
		return nil
	}
	out := make(map[string]*domain.RetryEntry, len(in))
	for k, v := range in {
		cp := *v
		out[k] = &cp
	}
	return out
}

func clonePRMap(in map[string]domain.PullRequest) map[string]domain.PullRequest {
	if in == nil {
		return nil
	}
	out := make(map[string]domain.PullRequest, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSpecsMap(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
