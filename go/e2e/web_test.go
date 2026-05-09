package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/web"
	"github.com/openai/symphony/go/internal/workspace"
)

// TestE2E_WebDashboard wires the orchestrator (memory tracker + mock runtime
// with a seeded issue) into the web handler via httptest.NewServer, then
// exercises every route and the WebSocket update stream end-to-end. This is
// the binary-level parity test for the §14 contract; the per-package tests
// in internal/web/ cover handler details, but only this one verifies the
// full stack including orchestrator OnUpdate fan-out.
//
// The optional Elixir-Go parity test (SYMPHONY_RUN_PARITY_TEST) is out of
// scope for v0 and is filed as future work.
func TestE2E_WebDashboard(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Polling.IntervalMs = 30
	cfg.Agent.MaxConcurrentAgents = 1
	cfg.Agent.MaxTurns = 1

	const seedID = "issue-web-1"
	const seedIdent = "WEB-1"
	issue := domain.Issue{
		ID:         seedID,
		Identifier: seedIdent,
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{TotalTokens: 7}}},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log).
		WithPromptTemplate("issue {{ issue.identifier }}")

	handler := web.NewHandler(orch)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)
	orchDone := make(chan error, 1)
	go func() { orchDone <- orch.Run(runCtx) }()

	// --- GET / ---
	t.Run("dashboard_html", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d want 200", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		// Required container ids per SPEC §14.4.
		for _, id := range []string{"metric-grid", "running-sessions", "retrying-sessions", "rate-limits", "header-status"} {
			if !strings.Contains(string(body), `id="`+id+`"`) {
				t.Errorf("dashboard body missing id=%q", id)
			}
		}
	})

	// --- GET /api/v1/state ---
	t.Run("api_state", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type: got %q", ct)
		}
		var snap observability.Snapshot
		if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		// generated_at must be a real timestamp (not zero) so consumers can
		// trust it as the snapshot watermark.
		if snap.GeneratedAt.IsZero() {
			t.Errorf("generated_at is zero")
		}
	})

	// --- GET /api/v1/missing → 404 with issue_not_found ---
	t.Run("api_issue_not_found", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/v1/does-not-exist")
		if err != nil {
			t.Fatalf("GET missing: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status: got %d want 404", resp.StatusCode)
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.Error.Code != "issue_not_found" {
			t.Errorf("error.code = %q, want issue_not_found", env.Error.Code)
		}
	})

	// --- POST /api/v1/refresh → 202 ---
	t.Run("api_refresh", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/api/v1/refresh", "application/json", nil)
		if err != nil {
			t.Fatalf("POST refresh: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status: got %d want 202", resp.StatusCode)
		}
	})

	// --- WebSocket: open, expect at least one fragment within 1s ---
	t.Run("ws_initial_frame", func(t *testing.T) {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer dialCancel()
		wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"
		conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })

		readCtx, readCancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer readCancel()
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if !bytes.Contains(data, []byte("hx-swap-oob")) {
			t.Errorf("ws frame missing OOB marker: %s", string(data))
		}
	})

	// Shut down orchestrator cleanly to verify lifecycle.
	runCancel()
	select {
	case <-orchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down within 2s")
	}
}
