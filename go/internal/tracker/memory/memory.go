// Package memory provides an in-memory Tracker implementation for testing per
// SPEC §5.3.1 ("memory" kind).
package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/symphony/go/internal/domain"
)

// Comment records a CreateComment call.
type Comment struct {
	IssueID string
	Body    string
}

// StateUpdate records an UpdateIssueState call.
type StateUpdate struct {
	IssueID string
	State   string
}

// MemoryTracker is an in-memory tracker backed by a simple slice.
type MemoryTracker struct {
	mu           sync.RWMutex
	issues       []domain.Issue
	Comments     []Comment
	StateUpdates []StateUpdate
	// ActiveStates is used to filter candidate issues; defaults to common active states.
	ActiveStates   []string
	TerminalStates []string
}

// New creates a new MemoryTracker seeded with the given issues.
func New(seed []domain.Issue) *MemoryTracker {
	issues := make([]domain.Issue, len(seed))
	copy(issues, seed)
	return &MemoryTracker{
		issues:         issues,
		ActiveStates:   []string{"todo", "in progress"},
		TerminalStates: []string{"done", "closed", "cancelled", "canceled", "duplicate"},
	}
}

// Seed adds issues to the tracker (useful for test setup).
func (t *MemoryTracker) Seed(issues ...domain.Issue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.issues = append(t.issues, issues...)
}

// FetchCandidateIssues returns issues whose state is in the active states.
func (t *MemoryTracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	activeSet := make(map[string]bool, len(t.ActiveStates))
	for _, s := range t.ActiveStates {
		activeSet[strings.ToLower(s)] = true
	}

	terminalSet := make(map[string]bool, len(t.TerminalStates))
	for _, s := range t.TerminalStates {
		terminalSet[strings.ToLower(s)] = true
	}

	var result []domain.Issue
	for _, issue := range t.issues {
		normalized := strings.ToLower(issue.State)
		if activeSet[normalized] && !terminalSet[normalized] {
			result = append(result, issue)
		}
	}
	return result, nil
}

// FetchIssuesByStates returns issues in the given states (used for startup cleanup).
func (t *MemoryTracker) FetchIssuesByStates(_ context.Context, states []string) ([]domain.Issue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	stateSet := make(map[string]bool, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = true
	}

	var result []domain.Issue
	for _, issue := range t.issues {
		if stateSet[strings.ToLower(issue.State)] {
			result = append(result, issue)
		}
	}
	return result, nil
}

// FetchIssueStatesByIDs returns the current state for each given issue ID
// (used for reconciliation).
func (t *MemoryTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]domain.Issue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	var result []domain.Issue
	for _, issue := range t.issues {
		if idSet[issue.ID] {
			result = append(result, issue)
		}
	}
	return result, nil
}

// CreateComment records the comment for the given issue ID.
func (t *MemoryTracker) CreateComment(_ context.Context, issueID, body string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.Comments = append(t.Comments, Comment{IssueID: issueID, Body: body})
	return nil
}

// UpdateIssueState updates the state of the given issue in memory.
func (t *MemoryTracker) UpdateIssueState(_ context.Context, issueID, state string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.StateUpdates = append(t.StateUpdates, StateUpdate{IssueID: issueID, State: state})

	for i := range t.issues {
		if t.issues[i].ID == issueID {
			t.issues[i].State = state
			return nil
		}
	}
	return fmt.Errorf("issue %q not found", issueID)
}

// GetIssue returns the issue with the given ID (test helper).
func (t *MemoryTracker) GetIssue(id string) (domain.Issue, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, issue := range t.issues {
		if issue.ID == id {
			return issue, true
		}
	}
	return domain.Issue{}, false
}
