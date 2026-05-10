// Package memory provides an in-memory Tracker implementation for testing per
// SPEC §5.3.1 ("memory" kind).
package memory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/durable"
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

	// nextID is the monotonic counter used to mint MEM-N identifiers when
	// CreateIssue is called without an explicit identifier.
	nextID int

	// specs is an in-process map of identifier -> OpenSpec body. Used by
	// SpecReader / SpecWriter; lost on restart unless a durable store is
	// attached.
	specs map[string]string

	// durable, when non-nil, persists issues + specs to disk on every
	// mutating call and is consulted on construction (preferring on-disk
	// state over the constructor seed).
	durable *durable.Store
}

// memoryDurableIssues is the wire shape persisted under
// "trackers/memory_issues".
type memoryDurableIssues struct {
	Issues []domain.Issue `json:"issues"`
	NextID int            `json:"next_id"`
}

// memoryDurableSpecs is the wire shape persisted under
// "trackers/memory_specs".
type memoryDurableSpecs struct {
	Specs map[string]string `json:"specs"`
}

// New creates a new MemoryTracker seeded with the given issues.
func New(seed []domain.Issue) *MemoryTracker {
	issues := make([]domain.Issue, len(seed))
	copy(issues, seed)
	return &MemoryTracker{
		issues:         issues,
		ActiveStates:   []string{"todo", "in progress"},
		TerminalStates: []string{"done", "closed", "cancelled", "canceled", "duplicate"},
		specs:          make(map[string]string),
	}
}

// NewWithDurable returns a MemoryTracker that mirrors issues + specs to s.
// On construction, an on-disk snapshot takes precedence over seed; when
// the snapshot is missing, seed is used as the initial state.
func NewWithDurable(seed []domain.Issue, s *durable.Store) (*MemoryTracker, error) {
	t := New(seed)
	t.durable = s
	if s == nil {
		return t, nil
	}

	var iss memoryDurableIssues
	err := s.Load("trackers/memory_issues", &iss)
	switch {
	case err == nil:
		t.issues = iss.Issues
		t.nextID = iss.NextID
	case errors.Is(err, os.ErrNotExist):
		// fresh install; keep seed
	default:
		return nil, fmt.Errorf("memory: load issues: %w", err)
	}

	var sp memoryDurableSpecs
	err = s.Load("trackers/memory_specs", &sp)
	switch {
	case err == nil:
		if sp.Specs != nil {
			t.specs = sp.Specs
		}
	case errors.Is(err, os.ErrNotExist):
		// fresh install
	default:
		return nil, fmt.Errorf("memory: load specs: %w", err)
	}

	return t, nil
}

// saveIssues persists the issues slice. Caller must hold t.mu (read OR write).
// Errors are logged via the durable store's caller, not propagated, to match
// the best-effort policy of the durable layer.
func (t *MemoryTracker) saveIssues() {
	if t.durable == nil {
		return
	}
	// Snapshot under lock-free copy semantics.
	issuesCopy := make([]domain.Issue, len(t.issues))
	copy(issuesCopy, t.issues)
	_ = t.durable.Save("trackers/memory_issues", memoryDurableIssues{ //nolint:errcheck
		Issues: issuesCopy,
		NextID: t.nextID,
	})
}

func (t *MemoryTracker) saveSpecs() {
	if t.durable == nil {
		return
	}
	specsCopy := make(map[string]string, len(t.specs))
	for k, v := range t.specs {
		specsCopy[k] = v
	}
	_ = t.durable.Save("trackers/memory_specs", memoryDurableSpecs{Specs: specsCopy}) //nolint:errcheck
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
			t.saveIssues()
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

// CreateIssue appends a new issue with an auto-generated MEM-N identifier.
func (t *MemoryTracker) CreateIssue(_ context.Context, draft domain.IssueDraft) (domain.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.nextID++
	id := fmt.Sprintf("MEM-%d", t.nextID)
	now := time.Now().UTC()
	state := "Todo"
	if len(t.ActiveStates) > 0 {
		state = t.ActiveStates[0]
	}
	issue := domain.Issue{
		ID:          id,
		Identifier:  id,
		Title:       strings.TrimSpace(draft.Title),
		Description: draft.Description,
		State:       state,
		Labels:      draft.Labels,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}
	t.issues = append(t.issues, issue)
	t.saveIssues()
	return issue, nil
}

// HasSpec returns whether a spec body has been written via WriteSpec.
func (t *MemoryTracker) HasSpec(_ context.Context, identifier string) (bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.specs[identifier]
	return ok, nil
}

// FetchAllIssues returns a copy of every tracked issue.
func (t *MemoryTracker) FetchAllIssues(_ context.Context) ([]domain.Issue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]domain.Issue, len(t.issues))
	copy(out, t.issues)
	return out, nil
}

// SetPullRequest sets the PR field on the issue with the given identifier.
func (t *MemoryTracker) SetPullRequest(_ context.Context, identifier string, pr domain.PullRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.issues {
		if t.issues[i].Identifier == identifier {
			cp := pr
			t.issues[i].PR = &cp
			t.saveIssues()
			return nil
		}
	}
	return fmt.Errorf("issue %q not found", identifier)
}

// ReadSpec returns the spec body previously written via WriteSpec.
func (t *MemoryTracker) ReadSpec(_ context.Context, identifier string) (string, string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	body, ok := t.specs[identifier]
	if !ok {
		return "", "", domain.ErrSpecNotFound
	}
	return body, etagFor(body), nil
}

// WriteSpec stores the spec body for the given identifier. ifMatchEtag is
// honored when non-empty: a mismatch returns domain.ErrSpecConflict.
func (t *MemoryTracker) WriteSpec(_ context.Context, identifier, body, ifMatchEtag string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ifMatchEtag != "" {
		current, ok := t.specs[identifier]
		if ok && etagFor(current) != ifMatchEtag {
			return "", domain.ErrSpecConflict
		}
	}
	t.specs[identifier] = body
	t.saveSpecs()
	return etagFor(body), nil
}

// etagFor returns a short content-derived etag.
func etagFor(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(body)))[:16]
}
