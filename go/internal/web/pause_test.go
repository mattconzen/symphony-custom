package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
)

type fakePauseCtl struct {
	pauseCalled, resumeCalled, cancelCalled string
	pauseErr, resumeErr, cancelErr          error
	state                                   string
	stateOK                                 bool
}

func (f *fakePauseCtl) RequestPause(id string) error {
	f.pauseCalled = id
	return f.pauseErr
}
func (f *fakePauseCtl) Resume(id string) error {
	f.resumeCalled = id
	return f.resumeErr
}
func (f *fakePauseCtl) RequestCancel(id string) error {
	f.cancelCalled = id
	return f.cancelErr
}
func (f *fakePauseCtl) RunStateOf(_ string) (string, bool) { return f.state, f.stateOK }

func handlerWithPauseCtl(p pauseSource) http.Handler {
	return newHandlerFromSource(
		&fakeSource{snap: observability.Snapshot{}, refreshQueued: true},
		p,
		nil,
		nil,
		nil,
		nil,
	)
}

func TestPause_Success(t *testing.T) {
	ctl := &fakePauseCtl{state: "pause_requested", stateOK: true}
	h := handlerWithPauseCtl(ctl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/WEB-1/pause", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "WEB-1", ctl.pauseCalled)
	var env pauseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.True(t, env.Queued)
	assert.Equal(t, "WEB-1", env.Identifier)
	assert.Equal(t, "pause_requested", env.State)
}

func TestPause_NotRunningReturns404(t *testing.T) {
	ctl := &fakePauseCtl{pauseErr: orchestrator.ErrNotRunning}
	h := handlerWithPauseCtl(ctl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UNKNOWN/pause", nil)
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestResume_NotPausedReturns409(t *testing.T) {
	ctl := &fakePauseCtl{resumeErr: orchestrator.ErrNotPaused}
	h := handlerWithPauseCtl(ctl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/WEB-1/resume", nil)
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestCancel_HappyPath(t *testing.T) {
	ctl := &fakePauseCtl{state: "cancel_requested", stateOK: true}
	h := handlerWithPauseCtl(ctl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/WEB-1/cancel", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "WEB-1", ctl.cancelCalled)
}

func TestPause_NilCtlReturns501(t *testing.T) {
	h := newHandlerFromSource(
		&fakeSource{snap: observability.Snapshot{}, refreshQueued: true},
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/WEB-1/pause", nil)
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
