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

// runEntry tracks an in-flight dispatch for one issue.
type runEntry struct {
	issue  domain.Issue
	cancel context.CancelFunc
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
	}
}

// WithPromptTemplate stores the workflow prompt template for use in first-turn
// prompt rendering.
func (o *Orchestrator) WithPromptTemplate(tmpl string) *Orchestrator {
	o.promptTemplate = tmpl
	return o
}

// Run starts the main poll loop. It returns when ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	// Store the orchestrator-level context so scheduleRetry can use it even
	// after a per-issue dispatch context has been cancelled.
	o.rootCtx = ctx

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
		}
	}
}

// tick is one poll iteration.
func (o *Orchestrator) tick(ctx context.Context) {
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
		o.running[issue.ID] = &runEntry{issue: issue, cancel: cancel}
		o.mu.Unlock()

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

	for _, issue := range updated {
		if terminalSet[strings.ToLower(issue.State)] {
			o.mu.Lock()
			if entry, ok := o.running[issue.ID]; ok {
				o.log.Info("reconcile: cancelling dispatch (terminal state)",
					"issue_id", issue.ID,
					"state", issue.State,
				)
				entry.cancel()
			}
			o.mu.Unlock()
		}
	}
}

// releaseClaim removes the claim and running entry for the given issue ID.
func (o *Orchestrator) releaseClaim(issueID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.claimed, issueID)
	delete(o.running, issueID)
}
