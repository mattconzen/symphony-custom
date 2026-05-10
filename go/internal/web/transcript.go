package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/openai/symphony/go/internal/transcript"
)

// transcriptResponse is the body of GET /api/v1/issues/{id}/transcript.
type transcriptResponse struct {
	Identifier string             `json:"identifier"`
	Events     []transcript.Event `json:"events"`
	NextFrom   int                `json:"next_from"`
	Complete   bool               `json:"complete"`
}

func (h *Handler) handleAPITranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if h.transcriptCtl == nil {
		writeAPIError(w, http.StatusNotImplemented, "not_supported", "transcript not wired")
		return
	}
	path := h.transcriptCtl.TranscriptPathForIdentifier(id)
	if path == "" {
		writeAPIError(w, http.StatusNotFound, "transcript_not_found", "no workspace root configured")
		return
	}

	from := parseIntQuery(r, "from", 0)
	limit := parseIntQuery(r, "limit", 200)
	if limit > 1000 {
		limit = 1000
	}

	events, next, complete, err := transcript.Read(path, from, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeAPIError(w, http.StatusNotFound, "transcript_not_found", "no transcript file for this issue yet")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "transcript_read_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(transcriptResponse{
		Identifier: id,
		Events:     events,
		NextFrom:   next,
		Complete:   complete,
	})
}

func parseIntQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
