package orchestrator_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

// buildCleanupCfg builds a config with a given workspace root.
func buildCleanupCfg(t *testing.T, wsRoot string) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Polling.IntervalMs = 30
	cfg.Workspace.Root = wsRoot
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	cfg.Hooks.TimeoutMs = 5_000
	return cfg
}

// TestStartupCleanup_HappyPath verifies that a workspace for a terminal-state
// issue is removed at startup, while the active issue's workspace is preserved.
func TestStartupCleanup_HappyPath(t *testing.T) {
	wsRoot := t.TempDir()
	cfg := buildCleanupCfg(t, wsRoot)

	doneIssue := domain.Issue{
		ID:         "done-1",
		Identifier: "DONE-1",
		State:      "Done",
	}
	activeIssue := domain.Issue{
		ID:         "active-1",
		Identifier: "ACTIVE-1",
		State:      "In Progress",
	}

	// Pre-create the Done issue's workspace directory.
	doneWsPath := filepath.Join(wsRoot, doneIssue.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath, 0755); err != nil {
		t.Fatalf("mkdir done workspace: %v", err)
	}
	// Also pre-create the active issue workspace (it should survive).
	activeWsPath := filepath.Join(wsRoot, activeIssue.WorkspaceKey())
	if err := os.MkdirAll(activeWsPath, 0755); err != nil {
		t.Fatalf("mkdir active workspace: %v", err)
	}

	tr := memory.New([]domain.Issue{doneIssue, activeIssue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- orch.Run(ctx)
	}()

	rec, ok := rt.(recorderIface)
	if !ok {
		t.Fatal("mock runtime does not implement recorderIface")
	}

	// Wait until the active issue is dispatched (signals that startup cleanup has run).
	eventually(t, 5*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// Assert: Done issue's workspace was removed.
	if _, err := os.Stat(doneWsPath); !os.IsNotExist(err) {
		t.Errorf("expected Done workspace %q to be removed, stat err = %v", doneWsPath, err)
	}

	// Assert: Active issue's pre-existing workspace still exists.
	if _, err := os.Stat(activeWsPath); err != nil {
		t.Errorf("expected active workspace %q to still exist, got err = %v", activeWsPath, err)
	}
}

// TestStartupCleanup_BeforeRemoveHookFires verifies that the before_remove hook
// runs during startup cleanup.
func TestStartupCleanup_BeforeRemoveHookFires(t *testing.T) {
	wsRoot := t.TempDir()
	cfg := buildCleanupCfg(t, wsRoot)

	doneIssue := domain.Issue{
		ID:         "done-hook",
		Identifier: "DONE-HOOK",
		State:      "Done",
	}
	activeIssue := domain.Issue{
		ID:         "active-hook",
		Identifier: "ACTIVE-HOOK",
		State:      "In Progress",
	}

	// Pre-create the Done issue's workspace directory.
	doneWsPath := filepath.Join(wsRoot, doneIssue.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath, 0755); err != nil {
		t.Fatalf("mkdir done workspace: %v", err)
	}

	// The marker file will be written to wsRoot (parent of workspace dir).
	markerFile := filepath.Join(wsRoot, doneIssue.WorkspaceKey()+".removed")

	// before_remove writes a marker file in the parent (wsRoot) using WORKSPACE_PATH env.
	cfg.Hooks.BeforeRemove = `touch "` + markerFile + `"`

	tr := memory.New([]domain.Issue{doneIssue, activeIssue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	log := observability.New(&bytes.Buffer{})
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	if !ok {
		t.Fatal("mock runtime does not implement recorderIface")
	}

	// Wait until active issue is dispatched.
	eventually(t, 5*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// Assert marker file exists (before_remove ran).
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("expected before_remove marker %q to exist, got err = %v", markerFile, err)
	}
}

// errorTracker is a Tracker that returns an error from FetchIssuesByStates.
type errorTracker struct {
	inner *memory.MemoryTracker
}

func (e *errorTracker) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	return e.inner.FetchCandidateIssues(ctx)
}
func (e *errorTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, errors.New("simulated tracker fetch error")
}
func (e *errorTracker) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	return e.inner.FetchIssueStatesByIDs(ctx, ids)
}
func (e *errorTracker) CreateComment(ctx context.Context, issueID, body string) error {
	return e.inner.CreateComment(ctx, issueID, body)
}
func (e *errorTracker) UpdateIssueState(ctx context.Context, issueID, state string) error {
	return e.inner.UpdateIssueState(ctx, issueID, state)
}

// TestStartupCleanup_BestEffortOnTrackerError verifies that if FetchIssuesByStates
// returns an error, the orchestrator logs and continues without crashing.
func TestStartupCleanup_BestEffortOnTrackerError(t *testing.T) {
	wsRoot := t.TempDir()
	cfg := buildCleanupCfg(t, wsRoot)

	activeIssue := domain.Issue{
		ID:         "active-err",
		Identifier: "ACTIVE-ERR",
		State:      "In Progress",
	}

	inner := memory.New([]domain.Issue{activeIssue})
	tr := &errorTracker{inner: inner}

	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	if !ok {
		t.Fatal("mock runtime does not implement recorderIface")
	}

	// Orchestrator should still dispatch the active issue despite tracker error.
	eventually(t, 5*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// No panic: if we got here, the orchestrator survived the error.
}

// TestStartupCleanup_BestEffortOnRemoveError verifies that if removing one workspace
// fails (e.g., read-only dir), the orchestrator logs and continues with others.
func TestStartupCleanup_BestEffortOnRemoveError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test read-only directory as root")
	}

	wsRoot := t.TempDir()
	cfg := buildCleanupCfg(t, wsRoot)

	doneIssue1 := domain.Issue{
		ID:         "done-ro",
		Identifier: "DONE-RO",
		State:      "Done",
	}
	doneIssue2 := domain.Issue{
		ID:         "done-ok",
		Identifier: "DONE-OK",
		State:      "Done",
	}
	activeIssue := domain.Issue{
		ID:         "active-ro",
		Identifier: "ACTIVE-RO",
		State:      "In Progress",
	}

	// Pre-create both Done workspaces.
	doneWsPath1 := filepath.Join(wsRoot, doneIssue1.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath1, 0755); err != nil {
		t.Fatalf("mkdir done workspace 1: %v", err)
	}
	// Put a file inside and make it read-only to prevent removal.
	innerFile := filepath.Join(doneWsPath1, "locked.txt")
	if err := os.WriteFile(innerFile, []byte("locked"), 0444); err != nil {
		t.Fatalf("write locked file: %v", err)
	}
	// Make the workspace dir itself read-only so RemoveAll fails.
	if err := os.Chmod(doneWsPath1, 0555); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	t.Cleanup(func() {
		// Restore permissions for cleanup.
		_ = os.Chmod(doneWsPath1, 0755)
		_ = os.Chmod(innerFile, 0644)
	})

	doneWsPath2 := filepath.Join(wsRoot, doneIssue2.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath2, 0755); err != nil {
		t.Fatalf("mkdir done workspace 2: %v", err)
	}

	tr := memory.New([]domain.Issue{doneIssue1, doneIssue2, activeIssue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	rec, ok := rt.(recorderIface)
	if !ok {
		t.Fatal("mock runtime does not implement recorderIface")
	}

	// The orchestrator must still dispatch the active issue (i.e., not crash).
	eventually(t, 5*time.Second, func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// The second Done workspace (without the permission issue) should be gone.
	if _, err := os.Stat(doneWsPath2); !os.IsNotExist(err) {
		t.Errorf("expected second Done workspace %q to be removed, stat err = %v", doneWsPath2, err)
	}
}
