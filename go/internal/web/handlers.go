package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/transcript"
)

// dashboardView is the render context for dashboard.html.tmpl. It embeds the
// snapshot (so the template's {{ .Counts }}, {{ .Running }}, etc. resolve
// directly) and adds extra fields the template needs.
type dashboardView struct {
	observability.Snapshot
	Error          *dashboardError
	CanCreateIssue bool
}

// issuePageView is the render context for issue.html.tmpl. The embedded
// issuePayload exposes the JSON-equivalent fields the template walks; the
// Transcript slice is server-side seeded from the per-issue JSONL file.
type issuePageView struct {
	issuePayload
	Transcript []transcript.Event
}

type dashboardError struct {
	Code    string
	Message string
}

// snapshotSource is the subset of *orchestrator.Orchestrator that the web
// handlers depend on. Keeping it narrow makes the handlers easy to test with
// fakes that don't need a full orchestrator wiring.
type snapshotSource interface {
	Snapshot() observability.Snapshot
	WorkspaceRoot() string
	RequestRefresh() bool
}

// transcriptSource exposes the orchestrator helper that maps an issue
// identifier to the on-disk transcript file. Implementations also return
// the live bus (when set).
type transcriptSource interface {
	TranscriptPathForIdentifier(identifier string) string
}

// pauseSource is the subset of *orchestrator.Orchestrator that the
// Pause/Resume/Cancel handlers depend on. Tests inject a fake.
type pauseSource interface {
	RequestPause(identifier string) error
	Resume(identifier string) error
	RequestCancel(identifier string) error
	RunStateOf(identifier string) (string, bool)
}

// trackerSource is the subset of tracker.Tracker that the write handlers
// (POST /api/v1/issues, PUT /api/v1/issues/{id}/spec, etc.) need. It is
// extracted so tests can supply lightweight fakes.
type trackerSource interface {
	CreateIssue(ctx context.Context, draft domain.IssueDraft) (domain.Issue, error)
	HasSpec(ctx context.Context, identifier string) (bool, error)
}

// Handler bundles the dashboard's HTTP surface area. Construct it via
// NewHandler and mount the returned http.Handler on an http.Server.
type Handler struct {
	orch          snapshotSource
	pauseCtl      pauseSource
	transcriptCtl transcriptSource
	transcriptBus *transcript.Bus
	trk           trackerSource
	specGen       SpecGenerator
	tmpl          *template.Template
	mux           *http.ServeMux
	broadcast     *broadcaster

	// specJobs tracks in-flight or completed spec-generation jobs.
	specJobsMu sync.Mutex
	specJobs   map[string]*specJob
}

// NewHandler returns the dashboard's http.Handler. The orchestrator is the
// source of truth for snapshot data; on each request that needs state, the
// handler calls orch.Snapshot() (the BuildSnapshot equivalent).
//
// The dashboard template is parsed once at construction time. Funcs() must
// be registered before ParseFS — the template references helpers like
// formatInt and ParseFS would otherwise fail.
//
// NewHandler also registers an OnUpdate callback on the orchestrator that
// fans out to every connected WebSocket subscriber. The callback overwrites
// any existing one — callers that need a custom update hook should compose
// theirs around the broadcaster (or call WithUpdateCallback after
// NewHandler returns, which will silently disable WS streaming).
func NewHandler(orch *orchestrator.Orchestrator) http.Handler {
	h := newHandlerFromSource(orch, orch, orch, orch.TranscriptBus(), nil, nil)
	orch.WithUpdateCallback(h.broadcast.broadcast)
	return h
}

// NewHandlerWithDeps is the full constructor used by main.go. trk and
// specGen are optional; when nil, the corresponding write endpoints
// respond with 405 unsupported.
func NewHandlerWithDeps(orch *orchestrator.Orchestrator, trk trackerSource, specGen SpecGenerator) http.Handler {
	h := newHandlerFromSource(orch, orch, orch, orch.TranscriptBus(), trk, specGen)
	orch.WithUpdateCallback(h.broadcast.broadcast)
	return h
}

func newHandlerFromSource(
	orch snapshotSource,
	pauseCtl pauseSource,
	transcriptCtl transcriptSource,
	transcriptBus *transcript.Bus,
	trk trackerSource,
	specGen SpecGenerator,
) *Handler {
	tmpl := template.Must(
		template.New("dashboard").
			Funcs(Funcs()).
			ParseFS(TemplatesFS, "templates/*.tmpl"),
	)

	h := &Handler{
		orch:          orch,
		pauseCtl:      pauseCtl,
		transcriptCtl: transcriptCtl,
		transcriptBus: transcriptBus,
		trk:           trk,
		specGen:       specGen,
		tmpl:          tmpl,
		mux:           http.NewServeMux(),
		broadcast:     newBroadcaster(),
		specJobs:      make(map[string]*specJob),
	}
	h.routes()
	return h
}

// ServeHTTP delegates to the registered mux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("/", h.handleDashboard)
	h.mux.HandleFunc("GET /static/{file}", h.handleStatic)
	h.mux.HandleFunc("GET /ws", h.handleWS)
	h.mux.HandleFunc("GET /issue/{issue_identifier}", h.handleIssuePage)
	h.mux.HandleFunc("/healthz", h.handleHealthz)
	h.mux.HandleFunc("GET /metrics", h.handleMetrics)

	// API routes mirror the Elixir router's explicit `match(:*, ...)` style:
	// dispatch on method inside one handler per path so 405 responses carry
	// the JSON error envelope rather than the mux's default empty body.
	h.mux.HandleFunc("/api/v1/state", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodGet: h.handleAPIState,
	}))
	h.mux.HandleFunc("/api/v1/refresh", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPIRefresh,
	}))
	h.mux.HandleFunc("/api/v1/{issue_identifier}", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodGet: h.handleAPIIssue,
	}))
	h.mux.HandleFunc("/api/v1/issues", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPICreateIssue,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/spec", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodGet: h.handleAPIReadSpec,
		http.MethodPut: h.handleAPIWriteSpec,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/spec/generate", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPIGenerateSpec,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/spec/generate/{job_id}", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodGet: h.handleAPIGenerateSpecStatus,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/pause", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPIPause,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/resume", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPIResume,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/cancel", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodPost: h.handleAPICancel,
	}))
	h.mux.HandleFunc("/api/v1/issues/{issue_identifier}/transcript", h.dispatchByMethod(map[string]http.HandlerFunc{
		http.MethodGet: h.handleAPITranscript,
	}))
}

// dispatchByMethod routes by HTTP method, returning the JSON method-not-
// allowed envelope for any unlisted method. Mirrors Elixir's
// match(:*, ...) routes per path.
func (h *Handler) dispatchByMethod(handlers map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fn, ok := handlers[r.Method]; ok {
			fn(w, r)
			return
		}
		h.handleAPIMethodNotAllowed(w, r)
	}
}

// handleDashboard renders the dashboard HTML page against the current
// orchestrator snapshot. The mux pattern is the unmethoded "/", which is
// the catch-all subtree, so this handler also has to filter unmatched
// paths and methods itself.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	view := dashboardView{
		Snapshot:       h.orch.Snapshot(),
		CanCreateIssue: h.canCreate(),
	}

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "dashboard.html.tmpl", view); err != nil {
		http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// handleHealthz returns a minimal liveness probe. The orchestrator running
// is sufficient — there is no deeper dependency check.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

// handleMetrics emits Prometheus text-format metrics derived from the
// current orchestrator snapshot. We hand-build the output rather than pull
// in a client library; the metric set is small and stable.
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := h.orch.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	pollingChecking := 0
	if snap.Polling.Checking {
		pollingChecking = 1
	}

	fmt.Fprintln(w, "# HELP symphony_running_sessions Number of issues currently running.")
	fmt.Fprintln(w, "# TYPE symphony_running_sessions gauge")
	fmt.Fprintf(w, "symphony_running_sessions %d\n", snap.Counts.Running)

	fmt.Fprintln(w, "# HELP symphony_retrying_sessions Number of issues currently waiting in the retry queue.")
	fmt.Fprintln(w, "# TYPE symphony_retrying_sessions gauge")
	fmt.Fprintf(w, "symphony_retrying_sessions %d\n", snap.Counts.Retrying)

	fmt.Fprintln(w, "# HELP symphony_agent_tokens_total Total agent tokens consumed by completed and active sessions.")
	fmt.Fprintln(w, "# TYPE symphony_agent_tokens_total counter")
	fmt.Fprintf(w, "symphony_agent_tokens_total{type=\"input\"} %d\n", snap.AgentTotals.InputTokens)
	fmt.Fprintf(w, "symphony_agent_tokens_total{type=\"output\"} %d\n", snap.AgentTotals.OutputTokens)

	fmt.Fprintln(w, "# HELP symphony_agent_seconds_running Total agent runtime seconds across completed and active sessions.")
	fmt.Fprintln(w, "# TYPE symphony_agent_seconds_running counter")
	fmt.Fprintf(w, "symphony_agent_seconds_running %d\n", snap.AgentTotals.SecondsRunning)

	fmt.Fprintln(w, "# HELP symphony_polling_checking 1 when the orchestrator is currently checking for work, 0 otherwise.")
	fmt.Fprintln(w, "# TYPE symphony_polling_checking gauge")
	fmt.Fprintf(w, "symphony_polling_checking %d\n", pollingChecking)

	fmt.Fprintln(w, "# HELP symphony_polling_interval_ms Configured polling interval in milliseconds.")
	fmt.Fprintln(w, "# TYPE symphony_polling_interval_ms gauge")
	fmt.Fprintf(w, "symphony_polling_interval_ms %d\n", snap.Polling.PollIntervalMs)
}

// handleIssuePage renders templates/issue.html.tmpl for the requested
// issue identifier. Returns 404 with an HTML "issue not found" body when
// neither a running nor a retry entry matches — JSON 404 stays scoped to
// /api/v1/{id}.
func (h *Handler) handleIssuePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issue_identifier")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	payload, ok := buildIssuePayload(h.orch.Snapshot(), id, h.orch.WorkspaceRoot())
	if !ok {
		var buf bytes.Buffer
		err := h.tmpl.ExecuteTemplate(&buf, "issue-not-found", map[string]any{"Identifier": id})
		if err != nil {
			http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(buf.Bytes())
		return
	}

	// Server-side seed the transcript section with the last 200 events.
	view := issuePageView{issuePayload: payload}
	if h.transcriptCtl != nil {
		path := h.transcriptCtl.TranscriptPathForIdentifier(id)
		if path != "" {
			if events, err := transcript.Tail(path, 200); err == nil {
				view.Transcript = events
			}
		}
	}

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "issue.html.tmpl", view); err != nil {
		http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// handleStatic serves embedded assets under /static/. Only files in the
// embedded static/ directory are reachable; path traversal attempts return
// 404. The Content-Type is derived from the file extension via
// mime.TypeByExtension so JS, CSS, and images all serve correctly.
func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		http.NotFound(w, r)
		return
	}
	// path.Clean defends against ".." even if ServeMux already filtered "/".
	if cleaned := path.Clean(name); cleaned != name || strings.HasPrefix(cleaned, ".") {
		http.NotFound(w, r)
		return
	}

	data, err := fs.ReadFile(StaticFS, "static/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}
