// Package orchestrator implements the poll-loop dispatch coordinator per SPEC §§7–8.
package orchestrator

import (
	"sort"
	"strings"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// chooseIssues selects which candidate issues should be dispatched in this tick.
// It respects global concurrency limits, per-state limits, the claimed set, and
// sorts by priority asc then created_at asc per SPEC §8.2.
//
// Parameters:
//   - candidates: issues returned by FetchCandidateIssues (already in active state).
//   - claimed: set of issue IDs already running or claimed.
//   - runningByState: count of currently running dispatches keyed by normalized state.
//   - cfg: agent concurrency config.
//
// Returns the subset of candidates that should be dispatched now.
func chooseIssues(
	candidates []domain.Issue,
	claimed map[string]struct{},
	runningByState map[string]int,
	cfg config.Config,
) []domain.Issue {
	// Filter out already-claimed issues and compute terminal sets.
	terminalSet := make(map[string]bool, len(cfg.Tracker.TerminalStates))
	for _, s := range cfg.Tracker.TerminalStates {
		terminalSet[strings.ToLower(s)] = true
	}

	eligible := make([]domain.Issue, 0, len(candidates))
	for _, issue := range candidates {
		if _, alreadyClaimed := claimed[issue.ID]; alreadyClaimed {
			continue
		}
		// Skip terminal-state issues that somehow made it through as candidates.
		if terminalSet[strings.ToLower(issue.State)] {
			continue
		}
		eligible = append(eligible, issue)
	}

	// Sort: priority asc (nil treated as lowest priority = max int), then created_at asc.
	sort.SliceStable(eligible, func(i, j int) bool {
		pi := priorityOf(eligible[i])
		pj := priorityOf(eligible[j])
		if pi != pj {
			return pi < pj
		}
		ci := createdAtMs(eligible[i])
		cj := createdAtMs(eligible[j])
		return ci < cj
	})

	// How many global slots remain?
	totalRunning := 0
	for _, n := range runningByState {
		totalRunning += n
	}
	globalSlots := cfg.Agent.MaxConcurrentAgents - totalRunning
	if globalSlots <= 0 {
		return nil
	}

	var chosen []domain.Issue
	// Track what we're about to add per state (so limits apply to this batch too).
	willAddByState := make(map[string]int)

	for _, issue := range eligible {
		if len(chosen) >= globalSlots {
			break
		}
		state := strings.ToLower(issue.State)
		// Check per-state limit if configured.
		if limit, ok := cfg.Agent.MaxConcurrentAgentsByState[state]; ok {
			currentForState := runningByState[state] + willAddByState[state]
			if currentForState >= limit {
				continue
			}
		}
		chosen = append(chosen, issue)
		willAddByState[state]++
	}

	return chosen
}

func priorityOf(issue domain.Issue) int {
	if issue.Priority == nil {
		return int(^uint(0) >> 1) // max int
	}
	return *issue.Priority
}

func createdAtMs(issue domain.Issue) int64 {
	if issue.CreatedAt == nil {
		return 0
	}
	return issue.CreatedAt.UnixMilli()
}
