package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/domain"
)

// SpecGenerator drafts an OpenSpec proposal.md body from an issue. The
// implementation invokes the configured agent runtime synchronously; the
// orchestrator owns the runtime so the wiring is done in cmd/symphony/main.go.
type SpecGenerator interface {
	Generate(ctx context.Context, issue domain.Issue) (string, error)
}

// SpecReadWriter is the union of the optional tracker.SpecReader and
// tracker.SpecWriter interfaces. Trackers that don't implement either return
// 405 from the spec endpoints.
type SpecReadWriter interface {
	ReadSpec(ctx context.Context, identifier string) (string, string, error)
	WriteSpec(ctx context.Context, identifier, body, ifMatchEtag string) (string, error)
}

// specJob tracks one in-flight or completed spec-generation invocation.
type specJob struct {
	mu       sync.Mutex
	identifier string
	body     string
	err      error
	done     bool
	startedAt time.Time
}

// canCreate is true iff a tracker is wired AND it supports CreateIssue. We
// detect support by attempting a no-op call... well, no, that would actually
// create. Instead we treat a configured trk as supporting create unless it
// returns ErrCreateUnsupported when the user POSTs.
func (h *Handler) canCreate() bool {
	return h.trk != nil
}

// handleAPICreateIssue accepts a JSON {title, description, labels?} body and
// creates a new issue via the configured tracker.
func (h *Handler) handleAPICreateIssue(w http.ResponseWriter, r *http.Request) {
	if h.trk == nil {
		writeAPIError(w, http.StatusMethodNotAllowed, "unsupported_create", "Active tracker does not support creating issues")
		return
	}

	var req struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	if len(req.Title) > 200 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "title must be <= 200 characters")
		return
	}

	issue, err := h.trk.CreateIssue(r.Context(), domain.IssueDraft{
		Title:       req.Title,
		Description: req.Description,
		Labels:      req.Labels,
	})
	if err != nil {
		if errors.Is(err, domain.ErrCreateUnsupported) {
			writeAPIError(w, http.StatusMethodNotAllowed, "unsupported_create", "Active tracker does not support creating issues")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "tracker_error", err.Error())
		return
	}

	// Trigger an immediate refresh so the new issue shows up.
	h.orch.RequestRefresh()

	writeJSON(w, http.StatusCreated, map[string]any{
		"issue_identifier": issue.Identifier,
		"issue_id":         issue.ID,
	})
}

// handleAPIReadSpec returns the OpenSpec body for the given identifier.
func (h *Handler) handleAPIReadSpec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	rw, ok := h.trk.(SpecReadWriter)
	if !ok {
		writeAPIError(w, http.StatusMethodNotAllowed, "unsupported_spec", "Active tracker does not support spec read/write")
		return
	}
	body, etag, err := rw.ReadSpec(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrSpecNotFound) {
			writeAPIError(w, http.StatusNotFound, "spec_not_found", "No spec for this issue")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "tracker_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identifier": id,
		"body":       body,
		"etag":       etag,
	})
}

// handleAPIWriteSpec writes a new spec body, optionally checking ifMatchEtag.
func (h *Handler) handleAPIWriteSpec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	rw, ok := h.trk.(SpecReadWriter)
	if !ok {
		writeAPIError(w, http.StatusMethodNotAllowed, "unsupported_spec", "Active tracker does not support spec read/write")
		return
	}
	var req struct {
		Body string `json:"body"`
		Etag string `json:"if_match_etag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	etag, err := rw.WriteSpec(r.Context(), id, req.Body, req.Etag)
	if err != nil {
		if errors.Is(err, domain.ErrSpecConflict) {
			writeAPIError(w, http.StatusConflict, "spec_conflict", "Stale etag; reload and retry")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "tracker_error", err.Error())
		return
	}
	h.orch.RequestRefresh()
	writeJSON(w, http.StatusOK, map[string]any{
		"identifier": id,
		"etag":       etag,
	})
}

// handleAPIGenerateSpec invokes the configured SpecGenerator for the issue
// and returns 202 Accepted with a job_id. The generated body is streamed
// back via /api/v1/issues/{id}/spec/generate/{job_id}.
func (h *Handler) handleAPIGenerateSpec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if h.specGen == nil {
		writeAPIError(w, http.StatusMethodNotAllowed, "unsupported_specgen", "No spec generator configured")
		return
	}

	jobID := newJobID()
	job := &specJob{identifier: id, startedAt: time.Now().UTC()}

	h.specJobsMu.Lock()
	h.specJobs[jobID] = job
	h.specJobsMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		body, err := h.specGen.Generate(ctx, domain.Issue{Identifier: id})
		job.mu.Lock()
		job.body = body
		job.err = err
		job.done = true
		job.mu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"identifier": id,
		"job_id":     jobID,
	})
}

// handleAPIGenerateSpecStatus returns the current state of a generation job.
// `status` is "pending" | "done" | "error". When done, body holds the draft.
func (h *Handler) handleAPIGenerateSpecStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	h.specJobsMu.Lock()
	job, ok := h.specJobs[jobID]
	h.specJobsMu.Unlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "job_not_found", "No spec-generation job with that ID")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	resp := map[string]any{
		"identifier": job.identifier,
		"job_id":     jobID,
		"started_at": job.startedAt.Format(time.RFC3339),
	}
	switch {
	case !job.done:
		resp["status"] = "pending"
	case job.err != nil:
		resp["status"] = "error"
		resp["error"] = job.err.Error()
	default:
		resp["status"] = "done"
		resp["body"] = job.body
	}
	writeJSON(w, http.StatusOK, resp)
}

func newJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
