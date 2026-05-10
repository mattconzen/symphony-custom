package orchestrator

import (
	"sort"
	"time"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
)

// BuildSnapshot projects the orchestrator's live state into the
// observability.Snapshot value consumed by the web dashboard and JSON API.
//
// NOTE: defined in the orchestrator package (rather than observability) because
// observability is imported by orchestrator — putting BuildSnapshot in
// observability would form an import cycle. Callers can use either
// `orchestrator.BuildSnapshot(o)` or the `o.Snapshot()` method; both return
// the same value.
//
// Field shape mirrors Elixir's SymphonyElixirWeb.Presenter.state_payload/2.
func BuildSnapshot(o *Orchestrator) observability.Snapshot {
	return o.Snapshot()
}

// Snapshot is the *Orchestrator method form of BuildSnapshot.
func (o *Orchestrator) Snapshot() observability.Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()

	running := make([]observability.RunningEntry, 0, len(o.running))
	for _, entry := range o.running {
		running = append(running, runEntryToSnapshot(entry))
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueIdentifier < running[j].IssueIdentifier
	})

	retrying := make([]observability.RetryEntry, 0, len(o.retryAttempts))
	for _, ra := range o.retryAttempts {
		retrying = append(retrying, retryEntryToSnapshot(ra))
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].IssueIdentifier < retrying[j].IssueIdentifier
	})

	return observability.Snapshot{
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Counts: observability.Counts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		CodexTotals: o.codexTotals,
		RateLimits:  o.rateLimits,
		Running:     running,
		Retrying:    retrying,
		Polling:     o.buildPolling(),
	}
}

// buildPolling derives the Polling projection. Caller must hold o.mu.
func (o *Orchestrator) buildPolling() observability.Polling {
	p := observability.Polling{
		Checking:       o.pollChecking,
		PollIntervalMs: o.cfg.Polling.IntervalMs,
	}
	if o.lastPollAt.IsZero() {
		return p
	}
	next := o.lastPollAt.Add(time.Duration(o.cfg.Polling.IntervalMs) * time.Millisecond)
	ms := int(time.Until(next).Milliseconds())
	if ms < 0 {
		ms = 0
	}
	p.NextPollInMs = ms
	return p
}

func runEntryToSnapshot(e *runEntry) observability.RunningEntry {
	out := observability.RunningEntry{
		IssueID:         e.issue.ID,
		IssueIdentifier: e.issue.Identifier,
		State:           e.issue.State,
		TurnCount:       e.turnCount,
		Tokens:          e.tokens,
	}
	if e.workspacePath != "" {
		wp := e.workspacePath
		out.WorkspacePath = &wp
	}
	if e.workerHost != "" {
		wh := e.workerHost
		out.WorkerHost = &wh
	}
	if e.sessionID != "" {
		sid := e.sessionID
		out.SessionID = &sid
	}
	if e.lastEvent != "" {
		ev := e.lastEvent
		out.LastEvent = &ev
	}
	if e.lastMessage != "" {
		msg := e.lastMessage
		out.LastMessage = &msg
	}
	if !e.startedAt.IsZero() {
		started := e.startedAt
		out.StartedAt = &started
	}
	if !e.lastEventAt.IsZero() {
		ev := e.lastEventAt
		out.LastEventAt = &ev
	}
	return out
}

func retryEntryToSnapshot(r *domain.RetryEntry) observability.RetryEntry {
	out := observability.RetryEntry{
		IssueID:         r.IssueID,
		IssueIdentifier: r.Identifier,
		Attempt:         r.Attempt,
	}
	if r.DueAtMs > 0 {
		due := time.UnixMilli(r.DueAtMs).UTC().Truncate(time.Second)
		out.DueAt = &due
	}
	if r.Error != "" {
		errMsg := r.Error
		out.Error = &errMsg
	}
	return out
}
