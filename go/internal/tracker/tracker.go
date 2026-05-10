// Package tracker defines the Tracker interface and factory per SPEC §11.1.
package tracker

import (
	"context"
	"fmt"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/tracker/jira"
	"github.com/openai/symphony/go/internal/tracker/linear"
	"github.com/openai/symphony/go/internal/tracker/markdown"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/tracker/openspec"
)

// IssueDraft is re-exported from domain for callers that work through the
// tracker package.
type IssueDraft = domain.IssueDraft

// ErrCreateUnsupported is re-exported from domain for callers that work
// through the tracker package.
var ErrCreateUnsupported = domain.ErrCreateUnsupported

// ErrSpecNotFound is re-exported from domain.
var ErrSpecNotFound = domain.ErrSpecNotFound

// ErrSpecConflict is re-exported from domain.
var ErrSpecConflict = domain.ErrSpecConflict

// Tracker is the interface that all tracker adapters must implement per SPEC §11.1.
type Tracker interface {
	FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error)
	FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error)
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
	CreateComment(ctx context.Context, issueID, body string) error
	UpdateIssueState(ctx context.Context, issueID, state string) error

	// CreateIssue persists a new issue. Implementations that do not support
	// creating issues MUST return domain.ErrCreateUnsupported.
	CreateIssue(ctx context.Context, draft domain.IssueDraft) (domain.Issue, error)

	// HasSpec reports whether the tracker has an OpenSpec-style spec
	// associated with the issue identifier. Used by the Kanban view to
	// partition Backlog vs Ready. Adapters without a spec concept return
	// (false, nil).
	HasSpec(ctx context.Context, identifier string) (bool, error)

	// FetchAllIssues returns every issue the tracker knows about, regardless
	// of state. Used by the Kanban derivation.
	FetchAllIssues(ctx context.Context) ([]domain.Issue, error)
}

// PullRequestSetter is implemented by trackers that can persist a PR link to
// their underlying storage. Adapters that only keep PR data in memory (or
// not at all) need not implement this interface.
type PullRequestSetter interface {
	SetPullRequest(ctx context.Context, identifier string, pr domain.PullRequest) error
}

// SpecReader is implemented by trackers that can return the OpenSpec-style
// spec body associated with an issue identifier.
type SpecReader interface {
	ReadSpec(ctx context.Context, identifier string) (body string, etag string, err error)
}

// SpecWriter is implemented by trackers that can persist a spec body for an
// issue identifier. Pass "" for ifMatchEtag to skip the optimistic-concurrency
// check.
type SpecWriter interface {
	WriteSpec(ctx context.Context, identifier string, body string, ifMatchEtag string) (newEtag string, err error)
}

// New returns a Tracker for the configured tracker kind.
func New(cfg config.Config) (Tracker, error) {
	switch cfg.Tracker.Kind {
	case "memory":
		return memory.New(nil), nil
	case "linear":
		return linear.New(cfg, nil)
	case "markdown":
		return markdown.New(cfg)
	case "openspec":
		return openspec.New(cfg)
	case "jira":
		return jira.New(cfg, nil)
	default:
		return nil, fmt.Errorf("unsupported tracker.kind: %s (will be added in a later phase)", cfg.Tracker.Kind)
	}
}

// NewWithSeed returns a memory tracker pre-seeded with the given issues.
func NewWithSeed(cfg config.Config, seed []domain.Issue) (Tracker, error) {
	switch cfg.Tracker.Kind {
	case "memory":
		return memory.New(seed), nil
	default:
		return nil, fmt.Errorf("unsupported tracker.kind: %s (will be added in a later phase)", cfg.Tracker.Kind)
	}
}
