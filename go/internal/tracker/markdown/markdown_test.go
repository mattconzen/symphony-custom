package markdown_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/tracker/markdown"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeCfg builds a minimal Config that points the markdown tracker at root.
func makeCfg(root string) config.Config {
	return config.Config{
		Tracker: config.Tracker{
			Kind:         "markdown",
			ActiveStates: []string{"Todo", "In Progress"},
			Markdown:     config.TrackerMarkdown{Root: root},
		},
	}
}

// writeFile writes content to a file under dir with the given name.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// absID returns the SHA-256 hex ID for the given absolute path, matching adapter.go logic.
func absID(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	h := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("%x", h)
}

// TestActiveFilter verifies that FetchCandidateIssues returns only issues
// whose front-matter state matches cfg.Tracker.ActiveStates (case-insensitive).
func TestActiveFilter(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "todo.md", "---\nstate: Todo\n---\n# Todo Issue\n")
	writeFile(t, dir, "inprogress.md", "---\nstate: In Progress\n---\n# In Progress Issue\n")
	writeFile(t, dir, "done.md", "---\nstate: Done\n---\n# Done Issue\n")

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	assert.Len(t, issues, 2, "should return only Todo and In Progress issues")

	states := make([]string, len(issues))
	for i, iss := range issues {
		states[i] = iss.State
	}
	assert.ElementsMatch(t, []string{"Todo", "In Progress"}, states)
}

// TestFilesystemAuthoritative verifies that FetchIssueStatesByIDs re-reads the
// file on every call (filesystem is the source of truth).
func TestFilesystemAuthoritative(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "task.md", "---\nstate: Todo\n---\n# Task\n")
	id := absID(t, path)

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	// First fetch: should be Todo.
	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{id})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Todo", issues[0].State)

	// Overwrite with Done.
	writeFile(t, dir, "task.md", "---\nstate: Done\n---\n# Task\n")

	// Second fetch: should now be Done — filesystem wins.
	issues, err = a.FetchIssueStatesByIDs(context.Background(), []string{id})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Done", issues[0].State)
}

// TestCreateCommentAppends verifies that CreateComment appends content and the
// file grows in size.
func TestCreateCommentAppends(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "issue.md", "---\nstate: Todo\n---\n# Issue\n")
	id := absID(t, path)

	before, err := os.Stat(path)
	require.NoError(t, err)

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	const commentBody = "hello world"
	require.NoError(t, a.CreateComment(context.Background(), id, commentBody))

	after, err := os.Stat(path)
	require.NoError(t, err)

	assert.Greater(t, after.Size(), before.Size(), "file size should have grown after CreateComment")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, "## Comment ")
	assert.Contains(t, content, commentBody)
}

// TestUpdateIssueStateAtomicPreservesBody verifies atomic state update and that
// the body bytes after the front-matter are preserved byte-for-byte.
func TestUpdateIssueStateAtomicPreservesBody(t *testing.T) {
	// The body is what appears after the closing --- fence (plus its trailing newline).
	// frontmatter.Parse strips the newline immediately after ---, so the body starts
	// at the first character of content (no leading newline).
	const body = "# My Issue\n\nThis is some **body** content.\n\nWith multiple paragraphs.\n"
	dir := t.TempDir()
	// Construct the full file: front-matter + newline after closing --- + body.
	original := "---\nstate: Todo\npriority: 2\n---\n" + body
	path := writeFile(t, dir, "myissue.md", original)
	id := absID(t, path)

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	require.NoError(t, a.UpdateIssueState(context.Background(), id, "Done"))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)

	// Front-matter must show state: Done.
	assert.Contains(t, content, "state: Done")
	// Must NOT contain old state value.
	assert.NotContains(t, content, "state: Todo")

	// Body bytes (after the closing ---\n) must be byte-identical.
	// Find the second "---" delimiter to locate where the body starts.
	// content starts with "---\n<yaml>---\n<body>"
	// Skip the first "---\n".
	rest := content[4:]
	closingIdx := strings.Index(rest, "\n---")
	require.GreaterOrEqual(t, closingIdx, 0, "closing front-matter delimiter not found")
	// closingIdx points at '\n' before '---', so body starts after "\n---\n" = closingIdx+5
	bodyStart := closingIdx + 5
	extractedBody := rest[bodyStart:]
	assert.Equal(t, body, extractedBody, "body content must be byte-identical after state update")
}

// TestConcurrentUpdateIssueState verifies that 10 goroutines calling
// UpdateIssueState concurrently leave the file in a parseable, correct state.
func TestConcurrentUpdateIssueState(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "concurrent.md", "---\nstate: Todo\n---\n# Concurrent\n")
	id := absID(t, path)

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = a.UpdateIssueState(context.Background(), id, "Done")
		}()
	}
	wg.Wait()

	// File must be parseable and state must be Done.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{id})
	require.NoError(t, err)
	require.Len(t, issues, 1, "file must still be parseable after concurrent updates")
	assert.Equal(t, "Done", issues[0].State)

	// File must start with "---" (valid front-matter).
	assert.True(t, strings.HasPrefix(string(raw), "---"), "file must start with YAML front-matter fence")
}

// TestFactorySelection verifies that tracker.New returns the markdown adapter
// when cfg.Tracker.Kind == "markdown".
func TestFactorySelection(t *testing.T) {
	dir := t.TempDir()
	// Need at least an existing directory for Preflight, but New itself just needs root != "".
	cfg := makeCfg(dir)
	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, tr)

	// Verify it's the markdown adapter by checking it satisfies the Tracker interface
	// and can actually fetch from the directory we provided.
	issues, err := tr.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	assert.Empty(t, issues, "empty dir should produce no issues")
}

// TestPreflightRejectsBadConfig verifies Preflight errors for invalid markdown config.
func TestPreflightRejectsBadConfig(t *testing.T) {
	t.Run("empty_root", func(t *testing.T) {
		cfg := config.Config{
			Tracker: config.Tracker{
				Kind:     "markdown",
				Markdown: config.TrackerMarkdown{Root: ""},
			},
			Agent: config.Agent{Runtime: "mock"},
		}
		err := config.Preflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tracker.markdown.root")
	})

	t.Run("nonexistent_root", func(t *testing.T) {
		cfg := config.Config{
			Tracker: config.Tracker{
				Kind:     "markdown",
				Markdown: config.TrackerMarkdown{Root: "/nonexistent/does/not/exist/xyz"},
			},
			Agent: config.Agent{Runtime: "mock"},
		}
		err := config.Preflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tracker.markdown.root")
	})
}

// TestFieldDerivation verifies the full field derivation per SPEC §11.6.2.
func TestFieldDerivation(t *testing.T) {
	dir := t.TempDir()
	content := "---\nstate: In Progress\npriority: 3\nlabels:\n  - BUG\n  - Feature\n---\n# My Feature\n\nSome description.\n"
	path := writeFile(t, dir, "my-feature.md", content)
	id := absID(t, path)

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1)

	iss := issues[0]
	assert.Equal(t, id, iss.ID, "id must be sha256 of abs path")
	assert.Equal(t, "my-feature", iss.Identifier, "identifier must be filename stem")
	assert.Equal(t, "My Feature", iss.Title, "title must be first H1")
	assert.Contains(t, iss.Description, "Some description.", "description must contain body text")
	assert.Equal(t, "In Progress", iss.State)
	assert.Equal(t, "file://"+filepath.Join(dir, "my-feature.md"), iss.URL)
	assert.ElementsMatch(t, []string{"bug", "feature"}, iss.Labels, "labels must be lowercased")
	require.NotNil(t, iss.Priority)
	assert.Equal(t, 3, *iss.Priority)
}

// TestNoFrontMatterDefaultsToTodo verifies that a file without front-matter
// gets state "Todo" by default.
func TestNoFrontMatterDefaultsToTodo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bare.md", "# Bare File\n\nNo front matter here.\n")

	a, err := markdown.New(makeCfg(dir))
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Todo", issues[0].State)
}
