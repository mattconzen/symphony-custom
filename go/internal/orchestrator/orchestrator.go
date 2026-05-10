// Package orchestrator implements the poll-loop dispatch coordinator per SPEC §§7–8.
package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/workspace"
)

// runEntry tracks an in-flight dispatch for one issue. Fields beyond
// (issue, cancel) feed the observability snapshot and mirror the per-running
// projection in Elixir's StatusDashboard / Presenter.
type runEntry struct {
	issue  domain.Issue
	cancel context.CancelFunc

	startedAt     time.Time
	sessionID     string
	workspacePath string
	workerHost    string
	turnCount     int
	lastEvent     string
	lastMessage   string
	lastEventAt   time.Time
	tokens        observability.EntryTokens
}

// Orchestrator is the poll-loop coordinator per SPEC §§7–8.
// It owns all runtime state guarded by a single mutex.
type Orchestrator struct {
	cfg     config.Config
	tracker tracker.Tracker
	runtime agent.Runtime
	ws      *workspace.Manager
	log     *observability.Logger

	// promptTemplate is the Liquid template body from WORKFLOW.md.
	// Set via WithPromptTemplate to decouple workflow loading from cfg.
	promptTemplate string

	// rootCtx is the context passed to Run(); used by scheduleRetry so that
	// the retry timer fires against the orchestrator lifetime, not the
	// (already-cancelled) per-issue dispatch context.
	rootCtx context.Context //nolint:containedctx

	mu            sync.Mutex
	running       map[string]*runEntry
	claimed       map[string]struct{}
	retryAttempts map[string]*domain.RetryEntry
	codexTotals   observability.TokenTotals
	rateLimits    any

	// pollChecking is true while a tracker fetch is in flight inside tick.
	// Surfaced via Snapshot.Polling.Checking. Guarded by mu.
	pollChecking bool
	// lastPollAt is the wall-clock time at which the most recent successful
	// poll completed. Zero before the first poll. Guarded by mu.
	lastPollAt time.Time

	// refreshC carries out-of-band refresh requests. Capacity 1 so a single
	// pending request coalesces additional calls. Read by Run's select; written
	// by RequestRefresh.
	refreshC chan struct{}

	// onUpdate is fired (outside the lock) whenever observable orchestrator
	// state changes — dispatch start, turn complete, dispatch finish,
	// reconcile state-change, retry scheduled. Mirrors the Elixir
	// StatusDashboard.notify_update / ObservabilityPubSub.broadcast_update
	// fan-out so the web dashboard can react without polling.
	onUpdate func()
}

// New returns an Orchestrator wired with the provided dependencies.
func New(
	cfg config.Config,
	t tracker.Tracker,
	rt agent.Runtime,
	ws *workspace.Manager,
	log *observability.Logger,
) *Orchestrator {
	return &Orchestrator{
		cfg:           cfg,
		tracker:       t,
		runtime:       rt,
		ws:            ws,
		log:           log,
		running:       make(map[string]*runEntry),
		claimed:       make(map[string]struct{}),
		retryAttempts: make(map[string]*domain.RetryEntry),
		refreshC:      make(chan struct{}, 1),
	}
}

// RequestRefresh schedules an immediate poll on the orchestrator. Returns true
// if a fresh refresh was queued, false if one is already pending (coalesced).
// Safe to call from any goroutine; non-blocking.
func (o *Orchestrator) RequestRefresh() bool {
	select {
	case o.refreshC <- struct{}{}:
		return true
	default:
		return false
	}
}

// WithPromptTemplate stores the workflow prompt template for use in first-turn
// prompt rendering.
func (o *Orchestrator) WithPromptTemplate(tmpl string) *Orchestrator {
	o.promptTemplate = tmpl
	return o
}

// WorkspaceRoot returns the configured workspace root directory. The web layer
// uses this to synthesize a workspace path for issues whose runtime entry has
// not yet recorded one (parity with Elixir's Presenter behaviour).
func (o *Orchestrator) WorkspaceRoot() string { return o.cfg.Workspace.Root }

// WithUpdateCallback registers cb to be invoked when observable orchestrator
// state changes. Pass nil to clear. The callback runs synchronously on the
// firing goroutine; keep it cheap (e.g. non-blocking channel send).
func (o *Orchestrator) WithUpdateCallback(cb func()) *Orchestrator {
	o.mu.Lock()
	o.onUpdate = cb
	o.mu.Unlock()
	return o
}

// notify fires the registered OnUpdate callback. Safe to call when no
// callback is set. Must be called with o.mu unlocked to avoid forcing
// callback handlers into the lock's critical section.
func (o *Orchestrator) notify() {
	o.mu.Lock()
	cb := o.onUpdate
	o.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// startupCleanup queries the tracker for terminal-state issues and removes
// their workspace directories per SPEC §8.6. Cleanup is best-effort: tracker
// fetch errors and per-issue remove errors are logged at warn level and
// execution continues. The method honours ctx cancellation mid-loop.
func (o *Orchestrator) startupCleanup(ctx context.Context) {
	terminalStates := o.cfg.Tracker.TerminalStates
	if len(terminalStates) == 0 {
		return
	}

	issues, err := o.tracker.FetchIssuesByStates(ctx, terminalStates)
	if err != nil {
		o.log.Warn("startup_cleanup: fetch terminal issues failed, skipping cleanup",
			"err", fmt.Sprintf("%v", err),
		)
		return
	}

	var removed, failed int
loop:
	for _, issue := range issues {
		// Honour context cancellation between removals.
		select {
		case <-ctx.Done():
			o.log.Warn("startup_cleanup: context cancelled mid-cleanup",
				"remaining", len(issues)-removed-failed,
			)
			break loop
		default:
		}

		if err := o.ws.RemoveForIssue(ctx, issue); err != nil {
			o.log.Warn("startup_cleanup: failed to remove workspace",
				"issue_id", issue.ID,
				"issue_identifier", issue.Identifier,
				"err", fmt.Sprintf("%v", err),
			)
			failed++
		} else {
			removed++
		}
	}

	o.log.Info("startup_cleanup_complete",
		"requested", len(issues),
		"removed", removed,
		"failed", failed,
	)
}

// Run starts the main poll loop. It returns when ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	// Store the orchestrator-level context so scheduleRetry can use it even
	// after a per-issue dispatch context has been cancelled.
	o.rootCtx = ctx

	// Perform one-shot startup cleanup before the first poll tick per SPEC §8.6.
	o.startupCleanup(ctx)

	interval := time.Duration(o.cfg.Polling.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run an initial tick immediately.
	o.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			o.tick(ctx)
		case <-o.refreshC:
			o.tick(ctx)
			ticker.Reset(interval)
		}
	}
}

// tick is one poll iteration.
func (o *Orchestrator) tick(ctx context.Context) {
	// Surface poll-in-flight state to Snapshot.Polling. Toggled around the
	// whole tick because reconcile + candidate fetch both hit the tracker.
	o.mu.Lock()
	o.pollChecking = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.pollChecking = false
		o.lastPollAt = time.Now()
		o.mu.Unlock()
	}()

	// 1. Re-validate preflight; skip dispatch on failure but keep reconciling.
	if err := config.Preflight(o.cfg); err != nil {
		o.log.Warn("preflight failed, skipping dispatch", "err", fmt.Sprintf("%v", err))
		o.reconcile(ctx)
		return
	}

	// 2. Reconcile running issues.
	o.reconcile(ctx)

	// 3. Fetch candidates.
	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.log.Warn("fetch candidates failed", "err", fmt.Sprintf("%v", err))
		return
	}

	// 4. Build current concurrency picture.
	o.mu.Lock()
	claimedSnapshot := make(map[string]struct{}, len(o.claimed))
	for id := range o.claimed {
		claimedSnapshot[id] = struct{}{}
	}
	runningByState := make(map[string]int)
	for _, entry := range o.running {
		state := strings.ToLower(entry.issue.State)
		runningByState[state]++
	}
	o.mu.Unlock()

	// 5. Choose issues to dispatch.
	chosen := chooseIssues(candidates, claimedSnapshot, runningByState, o.cfg)

	// 6. Claim and dispatch each chosen issue.
	for _, issue := range chosen {
		o.mu.Lock()
		// Re-check claim under the lock to avoid races.
		if _, alreadyClaimed := o.claimed[issue.ID]; alreadyClaimed {
			o.mu.Unlock()
			continue
		}
		o.claimed[issue.ID] = struct{}{}
		issueCtx, cancel := context.WithCancel(ctx)
		o.running[issue.ID] = &runEntry{
			issue:     issue,
			cancel:    cancel,
			startedAt: time.Now().UTC(),
		}
		o.mu.Unlock()

		// Mirrors Elixir handle_info(:run_poll_cycle) → notify_dashboard().
		o.notify()

		go func(iss domain.Issue, runCtx context.Context, cancelFn context.CancelFunc) {
			defer cancelFn()
			o.dispatchOne(runCtx, iss)
		}(issue, issueCtx, cancel)
	}
}

// reconcile refreshes state for all running issues. If an issue has moved to a
// terminal state, the per-issue context is cancelled per SPEC §8.5.
func (o *Orchestrator) reconcile(ctx context.Context) {
	o.mu.Lock()
	runningIDs := make([]string, 0, len(o.running))
	for id := range o.running {
		runningIDs = append(runningIDs, id)
	}
	o.mu.Unlock()

	if len(runningIDs) == 0 {
		return
	}

	updated, err := o.tracker.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		o.log.Warn("reconcile: fetch states failed", "err", fmt.Sprintf("%v", err))
		return
	}

	terminalSet := make(map[string]bool, len(o.cfg.Tracker.TerminalStates))
	for _, s := range o.cfg.Tracker.TerminalStates {
		terminalSet[strings.ToLower(s)] = true
	}

	stateChanged := false
	for _, issue := range updated {
		o.mu.Lock()
		entry, ok := o.running[issue.ID]
		if ok && entry.issue.State != issue.State {
			entry.issue.State = issue.State
			stateChanged = true
		}
		isTerminal := terminalSet[strings.ToLower(issue.State)]
		if isTerminal && ok {
			o.log.Info("reconcile: cancelling dispatch (terminal state)",
				"issue_id", issue.ID,
				"state", issue.State,
			)
			entry.cancel()
			stateChanged = true
		}
		o.mu.Unlock()
	}

	if stateChanged {
		// Mirrors Elixir reconcile path that ends in notify_dashboard().
		o.notify()
	}
}

// releaseClaim removes the claim and running entry for the given issue ID and
// rolls the entry's runtime seconds into the global codex_totals so the
// dashboard's cumulative runtime keeps growing across completed sessions
// (parity with Elixir record_session_completion_totals).
func (o *Orchestrator) releaseClaim(issueID string) {
	o.mu.Lock()
	if entry, ok := o.running[issueID]; ok && !entry.startedAt.IsZero() {
		secs := int(time.Since(entry.startedAt).Seconds())
		if secs > 0 {
			o.codexTotals.SecondsRunning += secs
		}
	}
	delete(o.claimed, issueID)
	delete(o.running, issueID)
	o.mu.Unlock()

	// Mirrors Elixir handle_info({:DOWN, ...}) → notify_dashboard().
	o.notify()
}
