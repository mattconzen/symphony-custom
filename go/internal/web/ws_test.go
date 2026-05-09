package web

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

func TestWS_StreamsInitialFrameAndUpdates(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Initial frame is sent on connect — verifies subscription wiring,
	// fragment rendering, and the OOB markup contract.
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	body := string(data)

	for _, want := range []string{
		`hx-swap-oob="innerHTML:#metric-grid"`,
		`hx-swap-oob="innerHTML:#running-sessions"`,
		`hx-swap-oob="innerHTML:#retrying-sessions"`,
		`hx-swap-oob="innerHTML:#rate-limits"`,
		`hx-swap-oob="innerHTML:#header-status"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("initial frame missing %q", want)
		}
	}

	// Trigger a broadcast and assert a second frame arrives.
	h.broadcast.broadcast()
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)
	if !bytes.Contains(data, []byte(`hx-swap-oob`)) {
		t.Errorf("update frame missing OOB markup")
	}
}

func TestWS_MultipleClientsReceiveBroadcast(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.CloseNow() })
		// Drain initial frame.
		_, _, err = c.Read(ctx)
		require.NoError(t, err)
		return c
	}

	a := dial()
	b := dial()

	h.broadcast.broadcast()

	for i, c := range []*websocket.Conn{a, b} {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("client %d: read after broadcast: %v", i, err)
		}
		if !bytes.Contains(data, []byte("hx-swap-oob")) {
			t.Errorf("client %d: payload missing OOB marker", i)
		}
	}
}

// TestWS_OrchestratorIntegration boots a full orchestrator (memory tracker
// + mock runtime), wires it through NewHandler, and verifies a WS client
// receives at least one fragment after a dispatch fires the OnUpdate
// callback. Mirrors the orchestrator_test.go idiom.
func TestWS_OrchestratorIntegration(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	cfg.Polling.IntervalMs = 50
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 1
	cfg.Agent.MaxTurns = 1
	cfg.Agent.MaxRetryBackoffMs = 1000
	cfg.Hooks.TimeoutMs = 5000

	issue := domain.Issue{
		ID:         "issue-ws",
		Identifier: "WS-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted, Tokens: agent.TokenUsage{TotalTokens: 1}}},
	})
	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log).
		WithPromptTemplate("issue {{ issue.identifier }}")

	handler := NewHandler(orch)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)
	done := make(chan error, 1)
	go func() { done <- orch.Run(runCtx) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })

	// Read at least two frames: the initial connect frame, then any frame
	// triggered by an orchestrator update during dispatch. A real dispatch
	// fires multiple OnUpdate callbacks (start, turn complete, finish), so
	// receiving one of them is enough.
	for i := 0; i < 2; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		if !bytes.Contains(data, []byte("hx-swap-oob")) {
			t.Errorf("frame %d missing OOB marker; body=%s", i, string(data))
		}
	}

	runCancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down within 2s")
	}
}
