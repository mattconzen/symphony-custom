package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/transcript"
)

type fakeTranscriptCtl struct{ path string }

func (f *fakeTranscriptCtl) TranscriptPathForIdentifier(_ string) string { return f.path }

func TestTranscript_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "session_started"}))
	require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "assistant_message"}))
	require.NoError(t, w.Close())

	h := newHandlerFromSource(
		&fakeSource{snap: observability.Snapshot{}, refreshQueued: true},
		nil,
		&fakeTranscriptCtl{path: path},
		nil,
		nil,
		nil,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/WEB-1/transcript", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body transcriptResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "WEB-1", body.Identifier)
	assert.Len(t, body.Events, 2)
	assert.True(t, body.Complete)
}

func TestTranscript_NotFound(t *testing.T) {
	h := newHandlerFromSource(
		&fakeSource{snap: observability.Snapshot{}, refreshQueued: true},
		nil,
		&fakeTranscriptCtl{path: filepath.Join(t.TempDir(), "missing.jsonl")},
		nil,
		nil,
		nil,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/UNKNOWN/transcript", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTranscript_PaginationParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	for i := 0; i < 30; i++ {
		require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "tick"}))
	}
	require.NoError(t, w.Close())

	h := newHandlerFromSource(
		&fakeSource{snap: observability.Snapshot{}, refreshQueued: true},
		nil,
		&fakeTranscriptCtl{path: path},
		nil,
		nil,
		nil,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/WEB-1/transcript?from=10&limit=5", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body transcriptResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Len(t, body.Events, 5)
	assert.Equal(t, 15, body.NextFrom)
	assert.False(t, body.Complete)
}
