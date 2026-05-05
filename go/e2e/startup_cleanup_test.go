// Package e2e: startup-cleanup scenario per SPEC §8.6.
package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/tracker/memory"
)

// TestE2E_StartupCleanup verifies that workspaces for terminal-state issues are
// removed at startup and that workspaces for active issues are preserved.
// The test uses a memory tracker seeded with one Done and one In-Progress issue,
// pre-creates workspace directories for both, runs the orchestrator briefly, then
// asserts the expected filesystem state.
func TestE2E_StartupCleanup(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Polling.IntervalMs = 20

	doneIssue := domain.Issue{
		ID:         "e2e-done-1",
		Identifier: "E2E-DONE-1",
		State:      "Done",
	}
	activeIssue := domain.Issue{
		ID:         "e2e-active-1",
		Identifier: "E2E-ACTIVE-1",
		State:      "In Progress",
	}

	wsRoot := cfg.Workspace.Root

	// Pre-create workspace directories for both issues.
	doneWsPath := filepath.Join(wsRoot, doneIssue.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath, 0755); err != nil {
		t.Fatalf("mkdir done workspace: %v", err)
	}
	// Drop a sentinel file so we can confirm the directory actually existed.
	sentinelFile := filepath.Join(doneWsPath, "sentinel.txt")
	if err := os.WriteFile(sentinelFile, []byte("old"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	activeWsPath := filepath.Join(wsRoot, activeIssue.WorkspaceKey())
	if err := os.MkdirAll(activeWsPath, 0755); err != nil {
		t.Fatalf("mkdir active workspace: %v", err)
	}

	tr := memory.New([]domain.Issue{doneIssue, activeIssue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	orch, _ := newOrch(t, cfg, tr, rt)
	cancel, done := runOrch(t, orch)

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock runtime does not implement recorderI")
	}

	// Wait until the active issue is dispatched — signals startup cleanup has run.
	waitFor(t, 5*time.Second, "active issue dispatched", func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// The Done workspace must have been removed.
	if _, err := os.Stat(doneWsPath); !os.IsNotExist(err) {
		t.Errorf("expected Done workspace %q to be removed after startup cleanup, stat err = %v", doneWsPath, err)
	}

	// The active workspace must still exist (pre-existing workspace preserved).
	if _, err := os.Stat(activeWsPath); err != nil {
		t.Errorf("expected active workspace %q to still exist, got err = %v", activeWsPath, err)
	}
}

// TestE2E_StartupCleanup_BeforeRemoveHook verifies that the before_remove hook
// fires during startup cleanup (SPEC §9.4).
func TestE2E_StartupCleanup_BeforeRemoveHook(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Polling.IntervalMs = 20

	doneIssue := domain.Issue{
		ID:         "e2e-done-hook",
		Identifier: "E2E-DONE-HOOK",
		State:      "Done",
	}
	activeIssue := domain.Issue{
		ID:         "e2e-active-hook",
		Identifier: "E2E-ACTIVE-HOOK",
		State:      "In Progress",
	}

	wsRoot := cfg.Workspace.Root

	// Pre-create the Done workspace directory.
	doneWsPath := filepath.Join(wsRoot, doneIssue.WorkspaceKey())
	if err := os.MkdirAll(doneWsPath, 0755); err != nil {
		t.Fatalf("mkdir done workspace: %v", err)
	}

	// The marker file will be created by the before_remove hook.
	markerFile := filepath.Join(wsRoot, doneIssue.WorkspaceKey()+".hook_ran")
	cfg.Hooks.BeforeRemove = `touch "` + markerFile + `"`

	tr := memory.New([]domain.Issue{doneIssue, activeIssue})
	rt := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	orch, _ := newOrch(t, cfg, tr, rt)
	cancel, done := runOrch(t, orch)

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock runtime does not implement recorderI")
	}

	waitFor(t, 5*time.Second, "active issue dispatched", func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// The before_remove hook must have created the marker file.
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("expected before_remove marker %q to exist, got err = %v", markerFile, err)
	}
}
