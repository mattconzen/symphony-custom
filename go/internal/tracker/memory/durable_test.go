package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/durable"
	"github.com/openai/symphony/go/internal/tracker/memory"
)

func TestMemoryTracker_DurableSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s1, err := durable.New(dir)
	require.NoError(t, err)

	first, err := memory.NewWithDurable(nil, s1)
	require.NoError(t, err)

	ctx := context.Background()
	issue, err := first.CreateIssue(ctx, domain.IssueDraft{Title: "First issue"})
	require.NoError(t, err)
	require.NoError(t, first.UpdateIssueState(ctx, issue.ID, "done"))

	_, err = first.WriteSpec(ctx, "WEB-1", "# spec body", "")
	require.NoError(t, err)

	// Open a second tracker against the same store; existing issues + specs
	// must be visible.
	s2, err := durable.New(dir)
	require.NoError(t, err)
	second, err := memory.NewWithDurable(nil, s2)
	require.NoError(t, err)

	issues, err := second.FetchAllIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "First issue", issues[0].Title)
	assert.Equal(t, "done", issues[0].State)

	body, _, err := second.ReadSpec(ctx, "WEB-1")
	require.NoError(t, err)
	assert.Equal(t, "# spec body", body)
}

func TestMemoryTracker_DurableSeedFallbackWhenMissing(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)
	seed := []domain.Issue{{ID: "S-1", Identifier: "SEED-1", State: "todo"}}
	tr, err := memory.NewWithDurable(seed, s)
	require.NoError(t, err)

	issues, err := tr.FetchAllIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "SEED-1", issues[0].Identifier)
}

func TestMemoryTracker_NilStoreReturnsPlainTracker(t *testing.T) {
	tr, err := memory.NewWithDurable(nil, nil)
	require.NoError(t, err)
	assert.NotNil(t, tr)
}
