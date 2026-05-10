package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/openai/symphony/go/internal/orchestrator"
)

// pauseEnvelope is the success-response body for the pause/resume/cancel
// endpoints. Mirrors the existing {queued,...} pattern used by the refresh
// endpoint so dashboard JS handles them uniformly.
type pauseEnvelope struct {
	Queued     bool   `json:"queued"`
	Identifier string `json:"identifier"`
	State      string `json:"state"`
}

func (h *Handler) handleAPIPause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if h.pauseCtl == nil {
		writeAPIError(w, http.StatusNotImplemented, "not_supported", "pause/resume/cancel not wired to the orchestrator")
		return
	}
	if err := h.pauseCtl.RequestPause(id); err != nil {
		writePauseError(w, err)
		return
	}
	state, _ := h.pauseCtl.RunStateOf(id)
	writePauseSuccess(w, http.StatusAccepted, id, state)
}

func (h *Handler) handleAPIResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if h.pauseCtl == nil {
		writeAPIError(w, http.StatusNotImplemented, "not_supported", "pause/resume/cancel not wired to the orchestrator")
		return
	}
	if err := h.pauseCtl.Resume(id); err != nil {
		writePauseError(w, err)
		return
	}
	state, _ := h.pauseCtl.RunStateOf(id)
	writePauseSuccess(w, http.StatusAccepted, id, state)
}

func (h *Handler) handleAPICancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if h.pauseCtl == nil {
		writeAPIError(w, http.StatusNotImplemented, "not_supported", "pause/resume/cancel not wired to the orchestrator")
		return
	}
	if err := h.pauseCtl.RequestCancel(id); err != nil {
		writePauseError(w, err)
		return
	}
	state, _ := h.pauseCtl.RunStateOf(id)
	writePauseSuccess(w, http.StatusAccepted, id, state)
}

func writePauseSuccess(w http.ResponseWriter, status int, id, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(pauseEnvelope{Queued: true, Identifier: id, State: state})
}

func writePauseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrNotRunning):
		writeAPIError(w, http.StatusNotFound, "not_running", err.Error())
	case errors.Is(err, orchestrator.ErrNotPaused):
		writeAPIError(w, http.StatusConflict, "not_paused", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
