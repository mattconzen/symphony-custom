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
}

// Handler bundles the dashboard's HTTP surface area. Construct it via
// NewHandler and mount the returned http.Handler on an http.Server.
type Handler struct {
	orch snapshotSource
	tmpl *template.Template
	mux  *http.ServeMux
}

// NewHandler returns the dashboard's http.Handler. The orchestrator is the
// source of truth for snapshot data; on each request that needs state, the
// handler calls orch.Snapshot() (the BuildSnapshot equivalent).
//
// The dashboard template is parsed once at construction time. Funcs() must
// be registered before ParseFS — the template references helpers like
// formatInt and ParseFS would otherwise fail.
func NewHandler(orch *orchestrator.Orchestrator) http.Handler {
	return newHandlerFromSource(orch)
}

func newHandlerFromSource(orch snapshotSource) *Handler {
	tmpl := template.Must(
		template.New("dashboard").
			Funcs(Funcs()).
			ParseFS(TemplatesFS, "templates/*.tmpl"),
	)

	h := &Handler{
		orch: orch,
		tmpl: tmpl,
		mux:  http.NewServeMux(),
	}
	h.routes()
	return h
}

// ServeHTTP delegates to the registered mux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /", h.handleDashboard)
	h.mux.HandleFunc("GET /static/{file}", h.handleStatic)
}

// handleDashboard renders the dashboard HTML page against the current
// orchestrator snapshot. Returns 404 for any GET path other than "/" so the
// catch-all "GET /" pattern doesn't swallow unrelated requests.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
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
