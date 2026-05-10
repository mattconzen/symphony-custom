package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
)

// dashboardView is the render context for dashboard.html.tmpl. It embeds the
// snapshot (so the template's {{ .Counts }}, {{ .Running }}, etc. resolve
// directly) and adds the Error sentinel the template's `{{ if .Error }}`
// branch expects. Error is always nil on the happy path.
type dashboardView struct {
	observability.Snapshot
	Error *dashboardError
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

// Handler bundles the dashboard's HTTP surface area. Construct it via
// NewHandler and mount the returned http.Handler on an http.Server.
type Handler struct {
	orch      snapshotSource
	tmpl      *template.Template
	mux       *http.ServeMux
	broadcast *broadcaster
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
	h := newHandlerFromSource(orch)
	orch.WithUpdateCallback(h.broadcast.broadcast)
	return h
}

func newHandlerFromSource(orch snapshotSource) *Handler {
	tmpl := template.Must(
		template.New("dashboard").
			Funcs(Funcs()).
			ParseFS(TemplatesFS, "templates/*.tmpl"),
	)

	h := &Handler{
		orch:      orch,
		tmpl:      tmpl,
		mux:       http.NewServeMux(),
		broadcast: newBroadcaster(),
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

	view := dashboardView{Snapshot: h.orch.Snapshot()}

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "dashboard.html.tmpl", view); err != nil {
		http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
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

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "issue.html.tmpl", payload); err != nil {
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
