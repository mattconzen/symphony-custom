package observability_test

import (
	"testing"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
)

func TestBuildKanban_Partitions(t *testing.T) {
	merged := domain.PullRequest{State: "merged", Number: 1}
	open := domain.PullRequest{State: "open", Number: 2}

	all := []domain.Issue{
		{ID: "a", Identifier: "A-1", Title: "no spec", State: "Todo"},
		{ID: "b", Identifier: "B-1", Title: "has spec", State: "Todo"},
		{ID: "c", Identifier: "C-1", Title: "running", State: "In Progress"},
		{ID: "d", Identifier: "D-1", Title: "review", State: "In Progress"},
		{ID: "e", Identifier: "E-1", Title: "merged", State: "In Progress"},
		{ID: "f", Identifier: "F-1", Title: "terminal", State: "Done"},
	}
	in := observability.KanbanInputs{
		AllIssues:   all,
		RunningIDs:  map[string]struct{}{"C-1": {}},
		RetryingIDs: map[string]struct{}{},
		SpecByID:    map[string]bool{"B-1": true},
		PRByID:      map[string]domain.PullRequest{"D-1": open, "E-1": merged},
		TerminalStates: []string{"Done"},
	}
	cols := observability.BuildKanban(in)

	want := map[string][]string{
		"backlog":     {"A-1"},
		"ready":       {"B-1"},
		"in_progress": {"C-1"},
		"in_review":   {"D-1"},
		"done":        {"E-1", "F-1"},
	}
	for _, col := range cols {
		w, ok := want[col.Key]
		if !ok {
			t.Errorf("unexpected column key %q", col.Key)
			continue
		}
		if len(col.Cards) != len(w) {
			t.Errorf("column %q: got %d cards want %d (%v)", col.Key, len(col.Cards), len(w), col.Cards)
			continue
		}
		for i, c := range col.Cards {
			if c.IssueIdentifier != w[i] {
				t.Errorf("column %q card %d: got %q want %q", col.Key, i, c.IssueIdentifier, w[i])
			}
		}
	}
}
