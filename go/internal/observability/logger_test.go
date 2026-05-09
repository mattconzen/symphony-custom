package observability_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
)

func TestLogger_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)
	log.Info("hello world")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m), "log output must be valid JSON")
	assert.Equal(t, "INFO", m["level"])
	assert.Equal(t, "hello world", m["msg"])
}

func TestLogger_WithIssueAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)

	now := time.Now()
	issue := domain.Issue{
		ID:         "issue-123",
		Identifier: "PROJ-42",
		Title:      "Fix the bug",
		CreatedAt:  &now,
	}
	log = log.WithIssue(issue)
	log.Info("dispatch started")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "issue-123", m["issue_id"])
	assert.Equal(t, "PROJ-42", m["issue_identifier"])
	assert.Equal(t, "dispatch started", m["msg"])
}

func TestLogger_WithSessionAttr(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)
	log = log.WithSession("sess-abc")
	log.Info("turn started")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "sess-abc", m["session_id"])
}

func TestLogger_WithAttemptAttr(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)
	log = log.WithAttempt(3)
	log.Warn("retrying")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.EqualValues(t, 3, m["attempt"])
	assert.Equal(t, "WARN", m["level"])
}

func TestLogger_ChainedAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := observability.New(&buf)

	now := time.Now()
	issue := domain.Issue{ID: "i1", Identifier: "X-1", CreatedAt: &now}
	log = log.WithIssue(issue).WithSession("s1").WithAttempt(1)
	log.Error("something failed", "err", "oops")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "i1", m["issue_id"])
	assert.Equal(t, "X-1", m["issue_identifier"])
	assert.Equal(t, "s1", m["session_id"])
	assert.EqualValues(t, 1, m["attempt"])
	assert.Equal(t, "ERROR", m["level"])
	assert.Equal(t, "oops", m["err"])
}
