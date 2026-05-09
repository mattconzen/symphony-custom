package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/openai/symphony/go/internal/observability"
)

// errorEnvelope mirrors Elixir's SymphonyElixirWeb.ObservabilityApiController
// error_response/4 shape: `{"error": {"code": "...", "message": "..."}}`.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// handleAPIState marshals the current snapshot. Mirrors Elixir
// Presenter.state_payload/2 success branch — the Go orchestrator does not
// have a snapshot timeout (snapshots are synchronous), so the timeout/
// unavailable error branches are not reachable here.
func (h *Handler) handleAPIState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.orch.Snapshot())
}

// handleAPIIssue returns the per-issue projection. 404 if the identifier is
// not present in either the running or retry slice. Field shape matches
// Elixir Presenter.issue_payload_body.
func (h *Handler) handleAPIIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	if id == "" {
		writeAPIError(w, http.StatusNotFound, "not_found", "Route not found")
		return
	}

	snap := h.orch.Snapshot()
	var running *observability.RunningEntry
	for i := range snap.Running {
		if snap.Running[i].IssueIdentifier == id {
			running = &snap.Running[i]
			break
		}
	}
	var retry *observability.RetryEntry
	for i := range snap.Retrying {
		if snap.Retrying[i].IssueIdentifier == id {
			retry = &snap.Retrying[i]
			break
		}
	}
	if running == nil && retry == nil {
		writeAPIError(w, http.StatusNotFound, "issue_not_found", "Issue not found")
		return
	}

	writeJSON(w, http.StatusOK, buildIssuePayload(id, running, retry))
}

// handleAPIRefresh acknowledges a refresh request. The Go orchestrator polls
// on its own tick loop; there is no out-of-band trigger plumbed yet, so the
// success envelope is informational. Returns 202 unconditionally; Elixir's
// :unavailable branch corresponds to a dead orchestrator process, which has
// no analogue here.
func (h *Handler) handleAPIRefresh(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"queued":       true,
		"coalesced":    false,
		"requested_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		"operations":   []string{"poll", "reconcile"},
	})
}

// handleAPIMethodNotAllowed returns 405 with the Elixir error envelope. Use
// it as the handler for `<METHOD> <path>` patterns that catch wrong methods.
func (h *Handler) handleAPIMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
}

// issueWorkspace mirrors Elixir's `workspace` map for the per-issue payload.
type issueWorkspace struct {
	Path *string `json:"path"`
	Host *string `json:"host"`
}

type issueAttempts struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type issueLogs struct {
	CodexSessionLogs []any `json:"codex_session_logs"`
}

type issueRecentEvent struct {
	At      *time.Time `json:"at"`
	Event   *string    `json:"event"`
	Message *string    `json:"message"`
}

type issueRunning struct {
	WorkerHost    *string                       `json:"worker_host"`
	WorkspacePath *string                       `json:"workspace_path"`
	SessionID     *string                       `json:"session_id"`
	TurnCount     int                           `json:"turn_count"`
	State         string                        `json:"state"`
	StartedAt     *time.Time                    `json:"started_at"`
	LastEvent     *string                       `json:"last_event"`
	LastMessage   *string                       `json:"last_message"`
	LastEventAt   *time.Time                    `json:"last_event_at"`
	Tokens        observability.EntryTokens     `json:"tokens"`
}

type issueRetry struct {
	Attempt       int        `json:"attempt"`
	DueAt         *time.Time `json:"due_at"`
	Error         *string    `json:"error"`
	WorkerHost    *string    `json:"worker_host"`
	WorkspacePath *string    `json:"workspace_path"`
}

type issuePayload struct {
	IssueIdentifier string             `json:"issue_identifier"`
	IssueID         string             `json:"issue_id"`
	Status          string             `json:"status"`
	Workspace       issueWorkspace     `json:"workspace"`
	Attempts        issueAttempts      `json:"attempts"`
	Running         *issueRunning      `json:"running"`
	Retry           *issueRetry        `json:"retry"`
	Logs            issueLogs          `json:"logs"`
	RecentEvents    []issueRecentEvent `json:"recent_events"`
	LastError       *string            `json:"last_error"`
	Tracked         map[string]any     `json:"tracked"`
}

func buildIssuePayload(id string, running *observability.RunningEntry, retry *observability.RetryEntry) issuePayload {
	out := issuePayload{
		IssueIdentifier: id,
		IssueID:         issueIDFromEntries(running, retry),
		Status:          issueStatus(running, retry),
		Workspace:       buildWorkspace(running, retry),
		Attempts:        buildAttempts(retry),
		Logs:            issueLogs{CodexSessionLogs: []any{}},
		RecentEvents:    buildRecentEvents(running),
		Tracked:         map[string]any{},
	}
	if running != nil {
		out.Running = &issueRunning{
			WorkerHost:    running.WorkerHost,
			WorkspacePath: running.WorkspacePath,
			SessionID:     running.SessionID,
			TurnCount:     running.TurnCount,
			State:         running.State,
			StartedAt:     running.StartedAt,
			LastEvent:     running.LastEvent,
			LastMessage:   running.LastMessage,
			LastEventAt:   running.LastEventAt,
			Tokens:        running.Tokens,
		}
	}
	if retry != nil {
		out.Retry = &issueRetry{
			Attempt:       retry.Attempt,
			DueAt:         retry.DueAt,
			Error:         retry.Error,
			WorkerHost:    retry.WorkerHost,
			WorkspacePath: retry.WorkspacePath,
		}
		out.LastError = retry.Error
	}
	return out
}

func issueIDFromEntries(running *observability.RunningEntry, retry *observability.RetryEntry) string {
	if running != nil {
		return running.IssueID
	}
	if retry != nil {
		return retry.IssueID
	}
	return ""
}

// issueStatus mirrors Elixir's logic: any running entry → "running"; only
// retry → "retrying".
func issueStatus(running *observability.RunningEntry, retry *observability.RetryEntry) string {
	if running != nil {
		return "running"
	}
	if retry != nil {
		return "retrying"
	}
	return ""
}

func buildAttempts(retry *observability.RetryEntry) issueAttempts {
	if retry == nil {
		return issueAttempts{}
	}
	attempt := retry.Attempt
	restart := attempt - 1
	if restart < 0 {
		restart = 0
	}
	return issueAttempts{RestartCount: restart, CurrentRetryAttempt: attempt}
}

// buildWorkspace prefers the running entry, then the retry entry. Unlike
// Elixir, we do not synthesize a workspace path from the configured root
// when both are nil — the dashboard is read-only and exposing the
// orchestrator's configured root through the web layer would couple
// internal/web to internal/config. Callers see `null` instead.
func buildWorkspace(running *observability.RunningEntry, retry *observability.RetryEntry) issueWorkspace {
	var path, host *string
	if running != nil {
		path = running.WorkspacePath
		host = running.WorkerHost
	}
	if path == nil && retry != nil {
		path = retry.WorkspacePath
	}
	if host == nil && retry != nil {
		host = retry.WorkerHost
	}
	return issueWorkspace{Path: path, Host: host}
}

func buildRecentEvents(running *observability.RunningEntry) []issueRecentEvent {
	if running == nil || running.LastEventAt == nil {
		return []issueRecentEvent{}
	}
	return []issueRecentEvent{{
		At:      running.LastEventAt,
		Event:   running.LastEvent,
		Message: running.LastMessage,
	}}
}
