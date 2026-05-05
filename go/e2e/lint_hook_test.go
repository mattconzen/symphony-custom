// Package e2e - golangci-lint between_turns hook test.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// findGolangciLint returns the path to golangci-lint, or "" if not found.
func findGolangciLint() string {
	p, err := exec.LookPath("golangci-lint")
	if err != nil {
		return ""
	}
	return p
}

// writeGoProject writes a minimal Go project with one deliberate lint violation
// into the given directory. Returns the module name and the go file path.
func writeGoProject(t *testing.T, dir string) {
	t.Helper()

	// go.mod
	goMod := `module example.com/linttest

go 1.21
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	// main.go with an unused import (errcheck-style violation that golangci-lint catches).
	// We use an unused variable which is a compile error, so instead use a
	// function that returns an error but the error is not checked (errcheck).
	// Actually, to avoid compile errors, let's use a simpler approach:
	// an exported function that shadows a builtin (revive: redefines-builtin-id).
	// The safest lint violation: unused variable inside a function won't compile.
	// Use: exported function name that is unexported but exported doc comment
	// triggers stylecheck. Simplest: nolint-free file with a known warning.
	//
	// Use fmt.Println with an unused return in a function - but that's fine.
	// Best safe bet: assign result of os.Open to blank but don't close it (resource leak).
	// Actually simplest: use "errcheck" - call os.Remove without checking error.
	mainGo := `package main

import (
	"fmt"
	"os"
)

func main() {
	// errcheck: result of os.Remove is not checked
	os.Remove("/tmp/doesnotexist_linttest_xyz")
	fmt.Println("hello")
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// Write a minimal .golangci.yml (v2 format) that only enables errcheck to make it fast.
	golangciYML := `version: "2"
linters:
  default: none
  enable:
    - errcheck
`
	if err := os.WriteFile(filepath.Join(dir, ".golangci.yml"), []byte(golangciYML), 0644); err != nil {
		t.Fatalf("write .golangci.yml: %v", err)
	}
}

// TestE2E_BetweenTurns_RealGolangciLint runs golangci-lint as a real between_turns
// hook against a tiny Go project with a deliberate lint violation. It verifies
// that the lint output appears in the next turn's continuation prompt.
func TestE2E_BetweenTurns_RealGolangciLint(t *testing.T) {
	lintPath := findGolangciLint()
	if lintPath == "" {
		t.Skip("golangci-lint not on $PATH; skipping real-lint between_turns test")
	}

	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Polling.IntervalMs = 50
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 1
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	// Hook: run golangci-lint in the workspace dir.
	// golangci-lint exits non-zero when it finds violations.
	cfg.Hooks.BetweenTurns = fmt.Sprintf("%s run 2>&1", lintPath)
	cfg.Hooks.TimeoutMs = 60_000

	issue := domain.Issue{
		ID:         "lint-1",
		Identifier: "LINT-1",
		State:      "In Progress",
	}

	// Pre-create the workspace directory and write a Go project with lint violations.
	wsPath := filepath.Join(cfg.Workspace.Root, issue.WorkspaceKey())
	if err := os.MkdirAll(wsPath, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	writeGoProject(t, wsPath)

	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
		},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock does not implement recorderI")
	}

	// Wait for at least 2 turns (hook fires between turn 1 and 2).
	end := time.Now().Add(90 * time.Second)
	for time.Now().Before(end) {
		if rec.RecordedTurnCount() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-done

	prompts := rec.RecordedPrompts()
	if len(prompts) < 2 {
		t.Fatalf("expected >= 2 recorded turns, got %d", len(prompts))
	}

	turn2Prompt := prompts[1].Prompt
	t.Logf("Turn 2 prompt (first 500 chars): %.500s", turn2Prompt)

	// The second turn's prompt must contain the "Validation feedback" heading.
	if !strings.Contains(turn2Prompt, "Validation feedback") {
		t.Errorf("turn 2 prompt missing 'Validation feedback' section")
	}

	// Must contain some lint output — golangci-lint outputs the file/line.
	// Look for "errcheck" or "main.go" in the feedback.
	hasLintOutput := strings.Contains(turn2Prompt, "errcheck") ||
		strings.Contains(turn2Prompt, "main.go") ||
		strings.Contains(turn2Prompt, "Error return value")
	if !hasLintOutput {
		t.Errorf("turn 2 prompt does not contain expected lint output; got:\n%s", turn2Prompt)
	}
}

// TestE2E_BetweenTurns_HookPassNoFeedback verifies that a between_turns hook
// that exits 0 (pass) does NOT inject any feedback into the next turn.
func TestE2E_BetweenTurns_HookPassNoFeedback(t *testing.T) {
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Polling.IntervalMs = 20
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 1
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	// Always-pass hook.
	cfg.Hooks.BetweenTurns = "true"
	cfg.Hooks.TimeoutMs = 5_000

	issue := domain.Issue{
		ID:         "hookpass-1",
		Identifier: "HKPASS-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns: []mock.TurnScript{
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
			{Status: agent.TurnCompleted},
		},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log)

	cancel, done := runOrch(t, orch)

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock does not implement recorderI")
	}
	waitFor(t, 10*time.Second, "3 turns completed", func() bool {
		return rec.RecordedTurnCount() >= 3
	})
	cancel()
	<-done

	prompts := rec.RecordedPrompts()
	if len(prompts) < 2 {
		t.Fatalf("expected >= 2 prompts, got %d", len(prompts))
	}

	turn2Prompt := prompts[1].Prompt
	// No validation feedback when hook passes.
	if strings.Contains(turn2Prompt, "Validation feedback") {
		t.Errorf("turn 2 prompt should NOT contain 'Validation feedback' when hook exits 0; got:\n%s", turn2Prompt)
	}

	// The continuation prompt should still have the continuation text.
	if !strings.Contains(turn2Prompt, "Continuation guidance") {
		t.Errorf("turn 2 prompt missing continuation guidance; got:\n%s", turn2Prompt)
	}
}
