package openspec_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/tracker/openspec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeCfg returns a minimal Config pointing the openspec tracker at root.
func makeCfg(root string) config.Config {
	return config.Config{
		Tracker: config.Tracker{
			Kind:           "openspec",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Closed", "Cancelled"},
			OpenSpec:       config.TrackerOpenSpec{Root: root},
		},
	}
}

// setupFixtures creates the canonical openspec fixture layout under root:
//
//	<root>/changes/add-foo/proposal.md  (labels: [api, internal], H1 title, body)
//	<root>/changes/add-foo/tasks.md     (## Tasks section)
//	<root>/changes/add-foo/design.md    (any content)
//	<root>/archive/old-bar/proposal.md  (simple content)
func setupFixtures(t *testing.T, root string) {
	t.Helper()

	changesDir := filepath.Join(root, "changes", "add-foo")
	require.NoError(t, os.MkdirAll(changesDir, 0o755))

	archiveDir := filepath.Join(root, "archive", "old-bar")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	// changes/add-foo/proposal.md
	proposal := `---
labels: [api, internal]
---
# Add Foo Feature

This proposal describes the foo feature.

It is a great idea.
`
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "proposal.md"), []byte(proposal), 0o644))

	// changes/add-foo/tasks.md
	tasks := `# Tasks for Add Foo

## Tasks
- [ ] step 1
- [ ] step 2

## Notes
Some notes here.
`
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "tasks.md"), []byte(tasks), 0o644))

	// changes/add-foo/design.md (adapter doesn't read this)
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "design.md"), []byte("# Design\n\nDesign notes.\n"), 0o644))

	// archive/old-bar/proposal.md
	archiveProposal := `---
labels: [legacy]
---
# Old Bar

An archived proposal.
`
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "proposal.md"), []byte(archiveProposal), 0o644))
}

// containsAll checks that all expected strings appear in the set (order independent).
func containsAll(t *testing.T, actual []string, expected ...string) {
	t.Helper()
	set := make(map[string]bool, len(actual))
	for _, s := range actual {
		set[s] = true
	}
	for _, e := range expected {
		assert.True(t, set[e], "expected label %q in %v", e, actual)
	}
}

// TestFetchCandidateIssues verifies that changes/ issues are returned with
// correct id, state, labels, and description.
func TestFetchCandidateIssues(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1, "should return exactly one active issue")

	iss := issues[0]
	assert.Equal(t, "add-foo", iss.ID)
	assert.Equal(t, "add-foo", iss.Identifier)
	assert.Equal(t, "Todo", iss.State)
	assert.Equal(t, "Add Foo Feature", iss.Title)

	// Labels: ["openspec", "api", "internal"] (order independent)
	containsAll(t, iss.Labels, "openspec", "api", "internal")

	// Description should contain the proposal body
	assert.Contains(t, iss.Description, "This proposal describes the foo feature.")

	// Description should also contain the tasks section
	assert.Contains(t, iss.Description, "step 1")
	assert.Contains(t, iss.Description, "step 2")

	// URL should be file:// pointing to proposal.md
	assert.True(t, strings.HasPrefix(iss.URL, "file://"), "URL should start with file://")
	assert.True(t, strings.HasSuffix(iss.URL, "proposal.md"), "URL should end with proposal.md")
}

// TestFetchIssueStatesByIDs verifies that each known slug returns the correct
// state and that missing slugs are omitted.
func TestFetchIssueStatesByIDs(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{"add-foo", "old-bar", "missing"})
	require.NoError(t, err)

	// Only add-foo and old-bar should be returned
	require.Len(t, issues, 2, "should return two issues (missing omitted)")

	byID := make(map[string]string, len(issues))
	for _, iss := range issues {
		byID[iss.ID] = iss.State
	}

	assert.Equal(t, "Todo", byID["add-foo"])
	assert.Equal(t, "Done", byID["old-bar"])
	_, hasMissing := byID["missing"]
	assert.False(t, hasMissing, "missing slug should be omitted")
}

// TestUpdateIssueState_TerminalAndBack verifies that moving an issue to a
// terminal state archives it and that the reverse restores it.
func TestUpdateIssueState_TerminalAndBack(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	// Confirm add-foo starts as Todo (in changes/)
	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{"add-foo"})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Todo", issues[0].State)

	// Move to Done (terminal) → should appear in archive/
	require.NoError(t, a.UpdateIssueState(context.Background(), "add-foo", "Done"))

	// Verify directory was moved
	assert.DirExists(t, filepath.Join(root, "archive", "add-foo"))
	assert.NoDirExists(t, filepath.Join(root, "changes", "add-foo"))

	// Re-fetch confirms state is Done
	issues, err = a.FetchIssueStatesByIDs(context.Background(), []string{"add-foo"})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Done", issues[0].State)

	// Reverse: move back to Todo (non-terminal) → should appear in changes/
	require.NoError(t, a.UpdateIssueState(context.Background(), "add-foo", "Todo"))

	assert.DirExists(t, filepath.Join(root, "changes", "add-foo"))
	assert.NoDirExists(t, filepath.Join(root, "archive", "add-foo"))

	issues, err = a.FetchIssueStatesByIDs(context.Background(), []string{"add-foo"})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Todo", issues[0].State)
}

// TestFetchIssueStatesByIDs_SentinelAutoArchives verifies SPEC §11.7: when
// changes/<slug>/.symphony-done is present, FetchIssueStatesByIDs MUST remove
// the sentinel, move the slug to archive/, and return state="Done" in the same
// call. This is the in-band completion signal a coding agent uses to tell the
// tracker "I'm done; archive me."
func TestFetchIssueStatesByIDs_SentinelAutoArchives(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	sentinel := filepath.Join(root, "changes", "add-foo", ".symphony-done")
	require.NoError(t, os.WriteFile(sentinel, nil, 0o644))

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{"add-foo"})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Done", issues[0].State)

	assert.NoDirExists(t, filepath.Join(root, "changes", "add-foo"))
	assert.DirExists(t, filepath.Join(root, "archive", "add-foo"))
	assert.NoFileExists(t, filepath.Join(root, "archive", "add-foo", ".symphony-done"))
}

// TestUpdateIssueState_NoOp verifies that moving an issue to its current
// state is a no-op (no error, no directory change).
func TestUpdateIssueState_NoOp(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	// add-foo is already in Todo → no-op
	require.NoError(t, a.UpdateIssueState(context.Background(), "add-foo", "Todo"))
	assert.DirExists(t, filepath.Join(root, "changes", "add-foo"))
}

// TestCreateComment verifies that commenting on an issue appends to proposal.md
// at the slug's current location.
func TestCreateComment(t *testing.T) {
	root := t.TempDir()
	setupFixtures(t, root)

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	// Comment on add-foo (currently in changes/)
	require.NoError(t, a.CreateComment(context.Background(), "add-foo", "hello"))

	data, err := os.ReadFile(filepath.Join(root, "changes", "add-foo", "proposal.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "## Comment ")
	assert.Contains(t, string(data), "hello")

	// After moving to Done, comment should append to archive location
	require.NoError(t, a.UpdateIssueState(context.Background(), "add-foo", "Done"))
	require.NoError(t, a.CreateComment(context.Background(), "add-foo", "archived comment"))

	data, err = os.ReadFile(filepath.Join(root, "archive", "add-foo", "proposal.md"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "archived comment")
}

// TestTrackerNew verifies that tracker.New returns an openspec adapter when
// cfg.Tracker.Kind == "openspec".
func TestTrackerNew(t *testing.T) {
	root := t.TempDir()
	// Create the root dir so it exists
	require.NoError(t, os.MkdirAll(root, 0o755))

	cfg := makeCfg(root)
	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

// TestPreflightRejectsNonExistentRoot verifies that Preflight rejects an
// openspec config pointing to a directory that doesn't exist.
func TestPreflightRejectsNonExistentRoot(t *testing.T) {
	cfg := config.Config{
		Tracker: config.Tracker{
			Kind:     "openspec",
			OpenSpec: config.TrackerOpenSpec{Root: "/nonexistent/path/that/does/not/exist"},
		},
		Agent: config.Agent{Runtime: "mock"},
		Hooks: config.Hooks{TimeoutMs: 60000},
	}

	err := config.Preflight(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accessible")
}

// TestFetchCandidateIssues_DescriptionContainsTasks verifies the description
// contains both the proposal body and the tasks section content.
func TestFetchCandidateIssues_DescriptionContainsTasks(t *testing.T) {
	root := t.TempDir()
	changesDir := filepath.Join(root, "changes", "test-slug")
	require.NoError(t, os.MkdirAll(changesDir, 0o755))

	proposal := `---
labels: [foo]
---
# Test Title

Proposal body text.
`
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "proposal.md"), []byte(proposal), 0o644))

	tasks := `## Tasks
- [ ] step 1
- [ ] step 2
`
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "tasks.md"), []byte(tasks), 0o644))

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1)

	desc := issues[0].Description
	assert.Contains(t, desc, "Proposal body text.")
	assert.Contains(t, desc, "step 1")
	assert.Contains(t, desc, "step 2")
}

// TestFetchCandidateIssues_NoTasksFile verifies that an issue without a
// tasks.md is still returned with just the proposal body as description.
func TestFetchCandidateIssues_NoTasksFile(t *testing.T) {
	root := t.TempDir()
	changesDir := filepath.Join(root, "changes", "no-tasks")
	require.NoError(t, os.MkdirAll(changesDir, 0o755))

	proposal := `---
labels: []
---
# No Tasks

Body without tasks.
`
	require.NoError(t, os.WriteFile(filepath.Join(changesDir, "proposal.md"), []byte(proposal), 0o644))

	a, err := openspec.New(makeCfg(root))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1)

	assert.Equal(t, "No Tasks", issues[0].Title)
	assert.Contains(t, issues[0].Description, "Body without tasks.")
}
