package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/symphony/go/internal/observability"
)

// fakeSource implements snapshotSource for handler tests without spinning up
// a full orchestrator. The dashboard template only reads fields off the
// snapshot, so a zero-valued Snapshot is enough to exercise the handler.
type fakeSource struct {
	snap            observability.Snapshot
	workspaceRoot   string
	refreshQueued   bool // value RequestRefresh returns
	refreshCalls    int
}

func (f *fakeSource) Snapshot() observability.Snapshot { return f.snap }
func (f *fakeSource) WorkspaceRoot() string            { return f.workspaceRoot }
func (f *fakeSource) RequestRefresh() bool {
	f.refreshCalls++
	return f.refreshQueued
}

func newTestHandler(snap observability.Snapshot) *Handler {
	return newHandlerFromSource(&fakeSource{snap: snap, refreshQueued: true})
}

func newTestHandlerWithRoot(snap observability.Snapshot, root string) *Handler {
	return newHandlerFromSource(&fakeSource{snap: snap, workspaceRoot: root, refreshQueued: true})
}

func TestHandleDashboard_RendersOK(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q want text/html prefix", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, marker := range []string{
		`id="metric-grid"`,
		`id="running-sessions"`,
		`id="retrying-sessions"`,
		`id="rate-limits"`,
		`id="header-status"`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
}

func TestDashboard_WSStatusPill(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, marker := range []string{
		`id="ws-status"`,
		`class="ws-status ws-status-live"`,
		`htmx:wsOpen`,
		`htmx:wsClose`,
		`htmx:wsError`,
		`htmx:wsConnecting`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
}

func TestDashboard_RefreshButton(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, marker := range []string{
		`class="refresh-button"`,
		`hx-post="/api/v1/refresh"`,
		`hx-swap="none"`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
}

func TestDashboard_PollingIndicator_Idle(t *testing.T) {
	t.Parallel()

	snap := observability.Snapshot{
		Polling: observability.Polling{Checking: false, NextPollInMs: 1500},
	}
	h := newTestHandler(snap)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, marker := range []string{
		`class="poll-badge poll-badge-idle"`,
		`Next poll in 1.5s`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
	if strings.Contains(string(body), "Checking") {
		t.Errorf("idle render unexpectedly contains 'Checking'")
	}
}

func TestDashboard_PollingIndicator_Checking(t *testing.T) {
	t.Parallel()

	snap := observability.Snapshot{
		Polling: observability.Polling{Checking: true},
	}
	h := newTestHandler(snap)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	for _, marker := range []string{
		`class="poll-badge poll-badge-checking"`,
		`Checking`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
}

func TestIssueDetailPage_Found(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/issue/TEST-1")
	if err != nil {
		t.Fatalf("GET issue page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q want text/html prefix", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, marker := range []string{
		`TEST-1`,
		`Session</h2>`,
		`Tokens</h2>`,
		`sess-1`,
		`/tmp/work/TEST-1`,
		`href="/"`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
	if strings.Contains(string(body), "Retry</h2>") {
		t.Errorf("running-only issue should not render Retry section")
	}
}

func TestIssueDetailPage_NotFound(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/issue/UNKNOWN-99")
	if err != nil {
		t.Fatalf("GET issue page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: got %q want text/html prefix", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, marker := range []string{
		`Issue not found`,
		`UNKNOWN-99`,
		`href="/"`,
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("body missing %q", marker)
		}
	}
}

func TestHandleDashboard_NonRootGETIs404(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d want 404", resp.StatusCode)
	}
}

func TestHandleStatic_ServesEmbeddedAssets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		path        string
		wantPrefix  string // expected Content-Type prefix
	}{
		{"htmx", "/static/htmx.min.js", "javascript"},
		{"htmx-ws", "/static/htmx-ws.min.js", "javascript"},
		{"css", "/static/dashboard.css", "text/css"},
	}

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d want 200", resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, tc.wantPrefix) {
				t.Errorf("Content-Type: got %q want substring %q", ct, tc.wantPrefix)
			}

			if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=3600" {
				t.Errorf("Cache-Control: got %q want %q", cc, "public, max-age=3600")
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if len(body) == 0 {
				t.Errorf("empty body")
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Errorf("body: got %q want %q", string(body), "ok\n")
	}

	postResp, err := http.Post(srv.URL+"/healthz", "", nil)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d want 405", postResp.StatusCode)
	}
}

func TestMetrics_Format(t *testing.T) {
	t.Parallel()

	snap := observability.Snapshot{
		Counts:      observability.Counts{Running: 3, Retrying: 2},
		CodexTotals: observability.TokenTotals{InputTokens: 1000, OutputTokens: 500, SecondsRunning: 90},
		Polling:     observability.Polling{Checking: true, PollIntervalMs: 2000},
	}
	h := newTestHandler(snap)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type: got %q want text/plain version=0.0.4", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)
	for _, marker := range []string{
		"# HELP symphony_running_sessions",
		"# TYPE symphony_running_sessions gauge",
		"symphony_running_sessions 3",
		"# HELP symphony_retrying_sessions",
		"# TYPE symphony_retrying_sessions gauge",
		"symphony_retrying_sessions 2",
		"# HELP symphony_codex_tokens_total",
		"# TYPE symphony_codex_tokens_total counter",
		`symphony_codex_tokens_total{type="input"} 1000`,
		`symphony_codex_tokens_total{type="output"} 500`,
		"# HELP symphony_codex_seconds_running",
		"# TYPE symphony_codex_seconds_running counter",
		"symphony_codex_seconds_running 90",
		"# HELP symphony_polling_checking",
		"# TYPE symphony_polling_checking gauge",
		"symphony_polling_checking 1",
		"# HELP symphony_polling_interval_ms",
		"# TYPE symphony_polling_interval_ms gauge",
		"symphony_polling_interval_ms 2000",
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("metrics body missing %q", marker)
		}
	}
}

func TestHandleStatic_RejectsTraversal(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})

	for _, p := range []string{
		"/static/",
		"/static/missing-asset.txt",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status got %d want 404", p, rec.Code)
		}
	}
}
