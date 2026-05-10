package orchestrator

import (
	"context"
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
	runningIDs := make(map[string]struct{}, len(o.running))
	totalRoles := len(o.cfg.Agent.Pipeline)
	for _, entry := range o.running {
		snap := runEntryToSnapshot(entry)
		if entry.pipeline != nil && totalRoles > 0 {
			snap.PipelineTotalRoles = totalRoles
		}
		running = append(running, snap)
		runningIDs[entry.issue.Identifier] = struct{}{}
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueIdentifier < running[j].IssueIdentifier
	})

	retrying := make([]observability.RetryEntry, 0, len(o.retryAttempts))
	retryingIDs := make(map[string]struct{}, len(o.retryAttempts))
	for _, ra := range o.retryAttempts {
		retrying = append(retrying, retryEntryToSnapshot(ra))
		retryingIDs[ra.Identifier] = struct{}{}
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].IssueIdentifier < retrying[j].IssueIdentifier
	})

	prByID := make(map[string]domain.PullRequest, len(o.pullRequests))
	for k, v := range o.pullRequests {
		prByID[k] = v
	}

	kanban := observability.BuildKanban(observability.KanbanInputs{
		AllIssues:      o.kanbanIssues,
		RunningIDs:     runningIDs,
		RetryingIDs:    retryingIDs,
		SpecByID:       o.kanbanSpecs,
		PRByID:         prByID,
		TerminalStates: o.cfg.Tracker.TerminalStates,
	})

	return observability.Snapshot{
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Counts: observability.Counts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		AgentTotals: o.agentTotals,
		RateLimits:  o.rateLimits,
		Running:     running,
		Retrying:    retrying,
		Polling:     o.buildPolling(),
		Kanban:      kanban,
	}
}

// refreshKanbanCache re-fetches all-issues + per-issue HasSpec from the
// tracker and stores the result for future Snapshot() calls. Called from
// the poll loop with the orchestrator-level context.
func (o *Orchestrator) refreshKanbanCache(ctx context.Context) {
	all, err := o.tracker.FetchAllIssues(ctx)
	if err != nil {
		o.log.Warn("kanban: FetchAllIssues failed", "err", err.Error())
		return
	}
	specs := make(map[string]bool, len(all))
	for _, iss := range all {
		has, err := o.tracker.HasSpec(ctx, iss.Identifier)
		if err != nil {
			continue
		}
		specs[iss.Identifier] = has
	}
	o.mu.Lock()
	o.kanbanIssues = all
	o.kanbanSpecs = specs
	o.mu.Unlock()
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
		RunState:        e.state.String(),
	}
	if e.pipeline != nil {
		out.PipelineRole = e.pipeline.CurrentRole
		out.PipelineCompleted = append([]string(nil), e.pipeline.CompletedRoles...)
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
