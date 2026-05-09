package memory_test

import (
	"context"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seed() []domain.Issue {
	return []domain.Issue{
		{ID: "1", Identifier: "ABC-1", Title: "First", State: "Todo"},
		{ID: "2", Identifier: "ABC-2", Title: "Second", State: "In Progress"},
		{ID: "3", Identifier: "ABC-3", Title: "Third", State: "Done"},
		{ID: "4", Identifier: "ABC-4", Title: "Fourth", State: "Closed"},
	}
}

func TestFetchCandidateIssues(t *testing.T) {
	mt := memory.New(seed())
	mt.ActiveStates = []string{"todo", "in progress"}
	mt.TerminalStates = []string{"done", "closed"}

	issues, err := mt.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	assert.Len(t, issues, 2)
	ids := []string{issues[0].ID, issues[1].ID}
	assert.ElementsMatch(t, []string{"1", "2"}, ids)
}

func TestFetchIssuesByStates(t *testing.T) {
	mt := memory.New(seed())

	issues, err := mt.FetchIssuesByStates(context.Background(), []string{"Done", "Closed"})
	require.NoError(t, err)

	assert.Len(t, issues, 2)
	ids := []string{issues[0].ID, issues[1].ID}
	assert.ElementsMatch(t, []string{"3", "4"}, ids)
}

func TestFetchIssueStatesByIDs(t *testing.T) {
	mt := memory.New(seed())

	issues, err := mt.FetchIssueStatesByIDs(context.Background(), []string{"1", "3"})
	require.NoError(t, err)

	assert.Len(t, issues, 2)
}

func TestCreateComment(t *testing.T) {
	mt := memory.New(seed())

	err := mt.CreateComment(context.Background(), "1", "Great work!")
	require.NoError(t, err)

	assert.Len(t, mt.Comments, 1)
	assert.Equal(t, "1", mt.Comments[0].IssueID)
	assert.Equal(t, "Great work!", mt.Comments[0].Body)
}

func TestUpdateIssueState(t *testing.T) {
	mt := memory.New(seed())

	err := mt.UpdateIssueState(context.Background(), "1", "In Progress")
	require.NoError(t, err)

	// State should be updated in memory
	issue, ok := mt.GetIssue("1")
	require.True(t, ok)
	assert.Equal(t, "In Progress", issue.State)

	assert.Len(t, mt.StateUpdates, 1)
	assert.Equal(t, "1", mt.StateUpdates[0].IssueID)
	assert.Equal(t, "In Progress", mt.StateUpdates[0].State)
}

func TestUpdateIssueState_NotFound(t *testing.T) {
	mt := memory.New(seed())

	err := mt.UpdateIssueState(context.Background(), "nonexistent", "Done")
	require.Error(t, err)
}

func TestTrackerNew_Memory(t *testing.T) {
	cfg := config.Config{
		Tracker: config.Tracker{Kind: "memory"},
	}
	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	assert.NotNil(t, tr)
}

func TestTrackerNew_Unsupported(t *testing.T) {
	cfg := config.Config{
		Tracker: config.Tracker{Kind: "unknown-kind"},
	}
	_, err := tracker.New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported tracker.kind")
}

func TestTrackerInterface_Compliance(t *testing.T) {
	// Verify MemoryTracker satisfies the tracker.Tracker interface
	var _ interface {
		FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error)
		FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error)
		FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
		CreateComment(ctx context.Context, issueID, body string) error
		UpdateIssueState(ctx context.Context, issueID, state string) error
	} = memory.New(nil)
}
