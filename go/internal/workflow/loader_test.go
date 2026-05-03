package workflow

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	td := filepath.Join("testdata")

	t.Run("with_front_matter", func(t *testing.T) {
		wf, err := Load(filepath.Join(td, "with_front_matter.md"))
		require.NoError(t, err)

		// Config map should contain parsed keys.
		assert.NotEmpty(t, wf.Config)

		trackerRaw, ok := wf.Config["tracker"]
		require.True(t, ok, "tracker key should be present")
		trackerMap, ok := trackerRaw.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "memory", trackerMap["kind"])

		// Prompt body should be trimmed.
		assert.Contains(t, wf.PromptTemplate, "issue.identifier")
		assert.Equal(t, wf.PromptTemplate, "You are working on issue {{ issue.identifier }}.")
	})

	t.Run("no_front_matter", func(t *testing.T) {
		wf, err := Load(filepath.Join(td, "no_front_matter.md"))
		require.NoError(t, err)

		assert.Empty(t, wf.Config)
		assert.Contains(t, wf.PromptTemplate, "No front matter here.")
		// Must be trimmed.
		assert.NotEqual(t, '\n', wf.PromptTemplate[len(wf.PromptTemplate)-1])
	})

	t.Run("non_map_yaml", func(t *testing.T) {
		_, err := Load(filepath.Join(td, "non_map.md"))
		assert.True(t, errors.Is(err, ErrFrontMatterNotMap), "expected ErrFrontMatterNotMap, got %v", err)
	})

	t.Run("missing_file", func(t *testing.T) {
		_, err := Load(filepath.Join(td, "missing.md"))
		assert.True(t, errors.Is(err, ErrMissingWorkflowFile), "expected ErrMissingWorkflowFile, got %v", err)
	})
}
