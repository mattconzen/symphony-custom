// Package e2e - CLI smoke tests.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildWorkflowFile writes a WORKFLOW.md fixture to dir and returns its path.
func buildWorkflowFile(t *testing.T, dir string, extra string) string {
	t.Helper()
	content := fmt.Sprintf(`---
tracker:
  kind: memory

polling:
  interval_ms: 50

workspace:
  root: %s

agent:
  runtime: mock
  max_concurrent_agents: 5
  max_turns: 1
%s
---
You are working on {{ issue.identifier }}.
`, filepath.Join(dir, "workspaces"), extra)
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write WORKFLOW.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	return path
}

// TestCLI_Smoke builds the binary, runs it against a fixture workflow, sends
// SIGTERM, and asserts clean exit with JSON log output.
func TestCLI_Smoke(t *testing.T) {
	skipIfNoBinary(t)

	dir := t.TempDir()
	wfPath := buildWorkflowFile(t, dir, "")

	cmd := exec.Command(binaryPath, "-workflow", wfPath)
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Capture stdout (JSON logs go there).
	logFile := filepath.Join(dir, "symphony.log")
	lf, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	defer lf.Close() //nolint:errcheck
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start symphony: %v", err)
	}

	// Give the orchestrator a moment to spin up and emit at least one tick.
	time.Sleep(300 * time.Millisecond)

	// Send SIGTERM.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}

	// Wait for exit with deadline.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("symphony exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("symphony did not exit within 5s after SIGTERM")
	}

	// Verify log output.
	lf.Close() //nolint:errcheck
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("no log output from symphony binary")
	}

	// Every non-empty line must be valid JSON.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d is not valid JSON: %q", i+1, line)
		}
	}

	// Must contain a "symphony started" message.
	found := false
	for _, line := range lines {
		if strings.Contains(line, "symphony started") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'symphony started' in log output; got:\n%s", raw)
	}
}

// TestCLI_SeedIssues verifies that SYMPHONY_SEED_ISSUES pre-populates the
// memory tracker so the orchestrator dispatches them and emits dispatch_started.
func TestCLI_SeedIssues(t *testing.T) {
	skipIfNoBinary(t)

	dir := t.TempDir()
	wfPath := buildWorkflowFile(t, dir, "")

	// Seed 3 issues.
	type seedIssue struct {
		ID         string `json:"ID"`
		Identifier string `json:"Identifier"`
		State      string `json:"State"`
		Title      string `json:"Title"`
	}
	seeds := []seedIssue{
		{ID: "seed-1", Identifier: "SEED-1", State: "In Progress", Title: "Issue 1"},
		{ID: "seed-2", Identifier: "SEED-2", State: "In Progress", Title: "Issue 2"},
		{ID: "seed-3", Identifier: "SEED-3", State: "In Progress", Title: "Issue 3"},
	}
	seedJSON, _ := json.Marshal(seeds)

	cmd := exec.Command(binaryPath, "-workflow", wfPath)
	cmd.Env = append(os.Environ(),
		"SYMPHONY_SEED_ISSUES="+string(seedJSON),
	)

	logFile := filepath.Join(dir, "symphony.log")
	lf, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	defer lf.Close() //nolint:errcheck
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start symphony: %v", err)
	}

	// Wait longer to allow dispatch of all 3 issues.
	time.Sleep(800 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("symphony did not exit within 5s after SIGTERM")
	}

	lf.Close() //nolint:errcheck
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	// Count dispatch_started messages.
	var dispatchCount int
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "dispatch started") {
			dispatchCount++
		}
	}

	// We expect all 3 issues dispatched.
	if dispatchCount < 3 {
		t.Errorf("expected >= 3 dispatch_started lines, got %d\nLog:\n%s", dispatchCount, raw)
	}
}

// TestCLI_InvalidWorkflow verifies the binary exits non-zero when the workflow
// file is missing.
func TestCLI_InvalidWorkflow(t *testing.T) {
	skipIfNoBinary(t)

	cmd := exec.Command(binaryPath, "-workflow", "/nonexistent/WORKFLOW.md")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected non-zero exit for missing workflow, got success; output:\n%s", out)
	}
}

// TestCLI_MarkdownTracker exercises the CLI with a real markdown tracker directory.
func TestCLI_MarkdownTracker(t *testing.T) {
	skipIfNoBinary(t)

	dir := t.TempDir()
	mdRoot := filepath.Join(dir, "issues")
	if err := os.MkdirAll(mdRoot, 0755); err != nil {
		t.Fatalf("mkdir issues: %v", err)
	}

	// Write 2 markdown issues.
	for i := 1; i <= 2; i++ {
		content := fmt.Sprintf("---\nstate: In Progress\n---\n# Issue %d\n\nWork to do.\n", i)
		path := filepath.Join(mdRoot, fmt.Sprintf("issue-%d.md", i))
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write issue %d: %v", i, err)
		}
	}

	wsRoot := filepath.Join(dir, "workspaces")
	wfContent := fmt.Sprintf(`---
tracker:
  kind: markdown
  active_states:
    - "In Progress"
  markdown:
    root: %s

polling:
  interval_ms: 50

workspace:
  root: %s

agent:
  runtime: mock
  max_concurrent_agents: 5
  max_turns: 1
---
Working on {{ issue.identifier }}.
`, mdRoot, wsRoot)

	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, []byte(wfContent), 0644); err != nil {
		t.Fatalf("write WORKFLOW.md: %v", err)
	}

	cmd := exec.Command(binaryPath, "-workflow", wfPath)

	logFile := filepath.Join(dir, "symphony.log")
	lf, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	defer lf.Close() //nolint:errcheck
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start symphony: %v", err)
	}

	// Allow enough time to dispatch both issues.
	time.Sleep(1 * time.Second)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("symphony did not exit within 5s")
	}
	lf.Close() //nolint:errcheck

	raw, _ := os.ReadFile(logFile)
	var dispatchCount int
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "dispatch started") {
			dispatchCount++
		}
	}
	if dispatchCount < 2 {
		t.Errorf("expected >= 2 dispatch_started for 2 markdown issues, got %d\nLog:\n%s", dispatchCount, raw)
	}
}
