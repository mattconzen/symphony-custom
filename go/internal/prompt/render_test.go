package prompt_test

import (
	"errors"
	"testing"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/prompt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender_AllFields(t *testing.T) {
	tmpl := "Issue {{ issue.identifier }}: {{ issue.title }} (state: {{ issue.state }})"
	issue := domain.Issue{
		ID:         "id-1",
		Identifier: "ABC-123",
		Title:      "Fix the bug",
		State:      "In Progress",
	}
	vars := prompt.Vars{
		Issue: issue,
	}

	out, err := prompt.Render(tmpl, vars)
	require.NoError(t, err)
	assert.Equal(t, "Issue ABC-123: Fix the bug (state: In Progress)", out)
}

func TestRender_WithAttempt(t *testing.T) {
	tmpl := "Attempt: {{ attempt }}"
	attempt := 2
	vars := prompt.Vars{
		Issue:   domain.Issue{Identifier: "X-1"},
		Attempt: &attempt,
	}

	out, err := prompt.Render(tmpl, vars)
	require.NoError(t, err)
	assert.Equal(t, "Attempt: 2", out)
}

func TestRender_UnknownVariable_Fails(t *testing.T) {
	tmpl := "Hello {{ unknown_var }}"
	vars := prompt.Vars{Issue: domain.Issue{Identifier: "X-1"}}

	_, err := prompt.Render(tmpl, vars)
	require.Error(t, err)
	assert.True(t, errors.Is(err, prompt.ErrTemplateRender))
}

func TestRender_IssueFields(t *testing.T) {
	pri := 2
	tmpl := "{{ issue.identifier }} priority={{ issue.priority }}"
	vars := prompt.Vars{
		Issue: domain.Issue{
			Identifier: "FOO-1",
			Priority:   &pri,
		},
	}
	out, err := prompt.Render(tmpl, vars)
	require.NoError(t, err)
	assert.Equal(t, "FOO-1 priority=2", out)
}

func TestRender_EmptyTemplate(t *testing.T) {
	out, err := prompt.Render("", prompt.Vars{Issue: domain.Issue{}})
	require.NoError(t, err)
	assert.Equal(t, "", out)
}
