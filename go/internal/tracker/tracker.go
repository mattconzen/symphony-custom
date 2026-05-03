// Package tracker defines the Tracker interface and factory per SPEC §11.1.
package tracker

import (
	"context"
	"fmt"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/tracker/linear"
	"github.com/openai/symphony/go/internal/tracker/markdown"
	"github.com/openai/symphony/go/internal/tracker/memory"
)

// Tracker is the interface that all tracker adapters must implement per SPEC §11.1.
type Tracker interface {
	FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error)
	FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error)
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
	CreateComment(ctx context.Context, issueID, body string) error
	UpdateIssueState(ctx context.Context, issueID, state string) error
}

// New returns a Tracker for the configured tracker kind. Supports "memory",
// "linear", and "markdown". Additional kinds will be registered in later phases.
func New(cfg config.Config) (Tracker, error) {
	switch cfg.Tracker.Kind {
	case "memory":
		return memory.New(nil), nil
	case "linear":
		return linear.New(cfg, nil)
	case "markdown":
		return markdown.New(cfg)
	default:
		return nil, fmt.Errorf("unsupported tracker.kind: %s (will be added in a later phase)", cfg.Tracker.Kind)
	}
}

// NewWithSeed returns a memory tracker pre-seeded with the given issues.
// This is used by tests and the CLI when SYMPHONY_SEED_ISSUES is set.
func NewWithSeed(cfg config.Config, seed []domain.Issue) (Tracker, error) {
	switch cfg.Tracker.Kind {
	case "memory":
		return memory.New(seed), nil
	default:
		return nil, fmt.Errorf("unsupported tracker.kind: %s (will be added in a later phase)", cfg.Tracker.Kind)
	}
}
