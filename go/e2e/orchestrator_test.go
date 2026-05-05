// Package e2e contains end-to-end tests that drive the orchestrator in-process
// across multiple subsystems.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/agent/mock"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/tracker/memory"
	"github.com/openai/symphony/go/internal/workspace"
)

// ---- helpers ----

func baseConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled", "Closed"}
	cfg.Polling.IntervalMs = 20
	cfg.Workspace.Root = t.TempDir()
	cfg.Agent.Runtime = "mock"
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 3
	cfg.Agent.MaxRetryBackoffMs = 300_000
	cfg.Hooks.TimeoutMs = 5_000
	return cfg
}

func newOrch(t *testing.T, cfg config.Config, tr tracker.Tracker, rt agent.Runtime) (*orchestrator.Orchestrator, *bytes.Buffer) {
	t.Helper()
	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	return orchestrator.New(cfg, tr, rt, wsMgr, log), &logBuf
}

// runOrch runs the orchestrator in a goroutine; returns cancel func and done chan.
func runOrch(t *testing.T, orch *orchestrator.Orchestrator) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	return cancel, done
}

// waitFor polls fn with 10ms intervals until it returns true or deadline expires.
func waitFor(t *testing.T, deadline time.Duration, msg string, fn func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// recorderI is the interface exposed by mock.Runtime for test introspection.
type recorderI interface {
	RecordedPrompts() []mock.RecordedTurn
	RecordedTurnCount() int
}

// ---- Scenario 1: Memory tracker × Mock runtime × multi-issue concurrency ----
// 5 issues, max_concurrent_agents=2 — peak concurrency must never exceed 2.

// peakConcurrentRuntime wraps a Runtime and tracks the maximum concurrent
// RunTurn calls observed.
type peakConcurrentRuntime struct {
	inner   agent.Runtime
	mu      sync.Mutex
	current int32
	peak    int32
	records []mock.RecordedTurn
}

func (p *peakConcurrentRuntime) StartSession(ctx context.Context, ws domain.Workspace) (agent.Session, error) {
	return p.inner.StartSession(ctx, ws)
}
func (p *peakConcurrentRuntime) StopSession(ctx context.Context, sess agent.Session) error {
	return p.inner.StopSession(ctx, sess)
}
func (p *peakConcurrentRuntime) RunTurn(ctx context.Context, sess agent.Session, prompt string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	cur := atomic.AddInt32(&p.current, 1)
	defer atomic.AddInt32(&p.current, -1)

	// Update peak under CAS.
	for {
		old := atomic.LoadInt32(&p.peak)
		if cur <= old {
			break
		}
		if atomic.CompareAndSwapInt32(&p.peak, old, cur) {
			break
		}
	}

	p.mu.Lock()
	p.records = append(p.records, mock.RecordedTurn{Prompt: prompt, Issue: issue})
	p.mu.Unlock()

	return p.inner.RunTurn(ctx, sess, prompt, issue, cb)
}
func (p *peakConcurrentRuntime) RecordedTurnCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

func TestE2E_MultiIssueConcurrencyLimit(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxConcurrentAgents = 2
	cfg.Agent.MaxTurns = 1

	issues := make([]domain.Issue, 5)
	for i := range issues {
		issues[i] = domain.Issue{
			ID:         fmt.Sprintf("issue-%d", i),
			Identifier: fmt.Sprintf("MC-%d", i),
			State:      "In Progress",
		}
	}
	tr := memory.New(issues)

	innerRT := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})
	peakRT := &peakConcurrentRuntime{inner: innerRT}

	orch, _ := newOrch(t, cfg, tr, peakRT)
	cancel, done := runOrch(t, orch)

	// Wait until all 5 issues have been processed (5 turn invocations).
	waitFor(t, 10*time.Second, "all 5 issues processed", func() bool {
		return peakRT.RecordedTurnCount() >= 5
	})
	cancel()
	<-done

	peak := atomic.LoadInt32(&peakRT.peak)
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", peak)
	}
	if peak < 1 {
		t.Errorf("peak concurrency = %d, expected at least 1 turn to run", peak)
	}
}

// ---- Scenario 2: Workspace path sanitization ----
// Issue identifier with path-traversal chars must stay under workspace root.

func TestE2E_WorkspaceSanitization(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxTurns = 1

	// Adversarial identifiers.
	adversarials := []string{
		"../../etc/passwd",
		"../secret",
		"has spaces here",
		"has/slash/inside",
		"unicode_中文",
	}

	wsRoot := cfg.Workspace.Root

	for _, id := range adversarials {
		issue := domain.Issue{
			ID:         id,
			Identifier: id,
			State:      "In Progress",
		}
		// WorkspaceKey is the sanitized path component.
		key := issue.WorkspaceKey()
		wsPath := filepath.Join(wsRoot, key)
		wsPath = filepath.Clean(wsPath)

		// Must be strictly under wsRoot.
		if !strings.HasPrefix(wsPath, wsRoot+string(filepath.Separator)) {
			t.Errorf("sanitized path %q is NOT under wsRoot %q for identifier %q", wsPath, wsRoot, id)
		}
	}
}

// TestE2E_WorkspaceSanitization_LiveDispatch dispatches an issue with a
// path-traversal identifier via the orchestrator and verifies the workspace
// path remains under root.
func TestE2E_WorkspaceSanitization_LiveDispatch(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxTurns = 1

	issue := domain.Issue{
		ID:         "path-traversal-1",
		Identifier: "../../etc/passwd",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
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
	waitFor(t, 5*time.Second, "turn ran", func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// The workspace directory should be under cfg.Workspace.Root.
	key := issue.WorkspaceKey() // ".._.._etc_passwd" or similar
	expectedPath := filepath.Join(cfg.Workspace.Root, key)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected workspace dir %q to exist, got: %v", expectedPath, err)
	}
	// And must NOT have escaped root.
	if !strings.HasPrefix(expectedPath, cfg.Workspace.Root+string(filepath.Separator)) {
		t.Errorf("workspace path %q escaped root %q", expectedPath, cfg.Workspace.Root)
	}
}

// ---- Scenario 3: Reconciliation race ----
// Start a blocking turn; mid-flight flip state to terminal; assert context
// cancelled within ~3 ticks.

type blockingRuntime struct {
	turnStarted chan struct{}
	turnDone    chan struct{}
}

func (b *blockingRuntime) StartSession(_ context.Context, _ domain.Workspace) (agent.Session, error) {
	return agent.Session{ID: "blocking-session"}, nil
}
func (b *blockingRuntime) StopSession(_ context.Context, _ agent.Session) error { return nil }
func (b *blockingRuntime) RunTurn(ctx context.Context, sess agent.Session, _ string, _ domain.Issue, _ agent.EventCallback) (agent.TurnResult, error) {
	select {
	case b.turnStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case b.turnDone <- struct{}{}:
	default:
	}
	return agent.TurnResult{Status: agent.TurnCancelled, SessionID: sess.ID}, ctx.Err()
}

func TestE2E_ReconciliationRace(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Polling.IntervalMs = 30

	issue := domain.Issue{
		ID:         "race-1",
		Identifier: "RACE-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})

	brt := &blockingRuntime{
		turnStarted: make(chan struct{}, 1),
		turnDone:    make(chan struct{}, 1),
	}

	orch, _ := newOrch(t, cfg, tr, brt)
	cancel, done := runOrch(t, orch)
	defer cancel()

	// Wait until the blocking turn has started.
	select {
	case <-brt.turnStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("RunTurn never started")
	}

	// Flip issue to terminal state mid-flight.
	if err := tr.UpdateIssueState(context.Background(), issue.ID, "Done"); err != nil {
		t.Fatalf("UpdateIssueState: %v", err)
	}

	// Allow ~10× tick interval for reconciliation to cancel the dispatch.
	deadline := time.Duration(cfg.Polling.IntervalMs*10) * time.Millisecond
	select {
	case <-brt.turnDone:
		// Success: context was cancelled.
	case <-time.After(deadline):
		t.Fatalf("dispatch context not cancelled within %v after state flip to terminal", deadline)
	}

	cancel()
	<-done
}

// ---- Scenario 4: Stress test — 50 issues, max_concurrent=5, -race ----

func TestE2E_Stress_50Issues(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in short mode")
	}

	cfg := baseConfig(t)
	cfg.Agent.MaxConcurrentAgents = 5
	cfg.Agent.MaxTurns = 1
	cfg.Polling.IntervalMs = 10

	const numIssues = 50
	issues := make([]domain.Issue, numIssues)
	for i := range issues {
		issues[i] = domain.Issue{
			ID:         fmt.Sprintf("stress-%d", i),
			Identifier: fmt.Sprintf("STR-%d", i),
			State:      "In Progress",
		}
	}
	tr := memory.New(issues)

	var turnCount int32
	innerRT := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})
	crt := &countingRuntime{inner: innerRT, count: &turnCount}

	orch, _ := newOrch(t, cfg, tr, crt)
	cancel, done := runOrch(t, orch)

	waitFor(t, 30*time.Second, "all 50 issues processed", func() bool {
		return atomic.LoadInt32(&turnCount) >= numIssues
	})
	cancel()
	<-done

	got := atomic.LoadInt32(&turnCount)
	if got < numIssues {
		t.Errorf("only %d/%d issues processed", got, numIssues)
	}
}

type countingRuntime struct {
	inner agent.Runtime
	count *int32
}

func (c *countingRuntime) StartSession(ctx context.Context, ws domain.Workspace) (agent.Session, error) {
	return c.inner.StartSession(ctx, ws)
}
func (c *countingRuntime) StopSession(ctx context.Context, sess agent.Session) error {
	return c.inner.StopSession(ctx, sess)
}
func (c *countingRuntime) RunTurn(ctx context.Context, sess agent.Session, prompt string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	res, err := c.inner.RunTurn(ctx, sess, prompt, issue, cb)
	if err == nil && res.Status == agent.TurnCompleted {
		atomic.AddInt32(c.count, 1)
	}
	return res, err
}

// ---- Scenario 5: Per-state concurrency cap ----

func TestE2E_PerStateConcurrencyCap(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 1
	cfg.Agent.MaxConcurrentAgentsByState = map[string]int{
		"in progress": 2,
	}

	// 6 "In Progress" issues — only 2 should run concurrently.
	issues := make([]domain.Issue, 6)
	for i := range issues {
		issues[i] = domain.Issue{
			ID:         fmt.Sprintf("psc-%d", i),
			Identifier: fmt.Sprintf("PSC-%d", i),
			State:      "In Progress",
		}
	}
	tr := memory.New(issues)

	innerRT := mock.New(mock.MockOpts{
		Turns: []mock.TurnScript{{Status: agent.TurnCompleted}},
	})
	peakRT := &peakConcurrentRuntime{inner: innerRT}

	orch, _ := newOrch(t, cfg, tr, peakRT)
	cancel, done := runOrch(t, orch)

	waitFor(t, 10*time.Second, "all 6 issues processed", func() bool {
		return peakRT.RecordedTurnCount() >= 6
	})
	cancel()
	<-done

	peak := atomic.LoadInt32(&peakRT.peak)
	if peak > 2 {
		t.Errorf("per-state peak concurrency = %d, want <= 2", peak)
	}
}

// ---- Scenario 6: OpenSpec tracker end-to-end ----
// Create fixture openspec dir → orchestrator processes issue → UpdateIssueState("Done")
// → directory moves to archive/.

func TestE2E_OpenSpecTracker(t *testing.T) {
	// Build a real openspec fixture directory.
	root := t.TempDir()
	changesDir := filepath.Join(root, "changes")
	slug := "my-proposal"
	proposalDir := filepath.Join(changesDir, slug)
	if err := os.MkdirAll(proposalDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	proposalContent := "---\ntitle: My Proposal\n---\n# My Proposal\n\nFix the thing.\n"
	if err := os.WriteFile(filepath.Join(proposalDir, "proposal.md"), []byte(proposalContent), 0644); err != nil {
		t.Fatalf("write proposal.md: %v", err)
	}

	cfg := baseConfig(t)
	cfg.Tracker.Kind = "openspec"
	cfg.Tracker.OpenSpec.Root = root
	cfg.Tracker.ActiveStates = []string{"Todo"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Agent.MaxTurns = 1

	// Build the openspec tracker via the factory.
	osTr, err := tracker.New(cfg)
	if err != nil {
		t.Fatalf("tracker.New: %v", err)
	}

	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)
	orch := orchestrator.New(cfg, osTr, rt, wsMgr, log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock runtime does not implement recorderI")
	}
	waitFor(t, 5*time.Second, "turn ran", func() bool {
		return rec.RecordedTurnCount() >= 1
	})

	// Call UpdateIssueState to move it to archive/.
	if err := osTr.UpdateIssueState(ctx, slug, "Done"); err != nil {
		t.Fatalf("UpdateIssueState: %v", err)
	}

	cancel()
	<-done

	// Verify the directory moved to archive/.
	archiveDir := filepath.Join(root, "archive", slug)
	if info, err := os.Stat(archiveDir); err != nil || !info.IsDir() {
		t.Errorf("expected archive dir %q to exist after Done, err=%v", archiveDir, err)
	}
	// And the changes dir should no longer have it.
	if _, err := os.Stat(proposalDir); !os.IsNotExist(err) {
		t.Errorf("expected changes/%s to be gone after Done, err=%v", slug, err)
	}
}

// ---- Scenario 7: JSON log output verification ----
// Verifies the orchestrator emits JSON logs with expected keys.

func TestE2E_JSONLogOutput(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxTurns = 1

	issue := domain.Issue{
		ID:         "log-test-1",
		Identifier: "LOG-1",
		State:      "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
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
	waitFor(t, 5*time.Second, "turn ran", func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	// Parse each log line as JSON and verify structure.
	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("no log output")
	}

	var foundDispatch bool
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("log line is not valid JSON: %q, err=%v", line, err)
			continue
		}
		// Every line must have "time", "level", "msg".
		for _, key := range []string{"time", "level", "msg"} {
			if _, ok := entry[key]; !ok {
				t.Errorf("log line missing key %q: %s", key, line)
			}
		}
		if msg, _ := entry["msg"].(string); strings.Contains(msg, "dispatch started") {
			foundDispatch = true
			// Must carry issue_id and issue_identifier.
			for _, key := range []string{"issue_id", "issue_identifier"} {
				if _, ok := entry[key]; !ok {
					t.Errorf("dispatch_started log missing key %q: %s", key, line)
				}
			}
		}
	}
	if !foundDispatch {
		t.Error("expected at least one 'dispatch started' log line")
	}
}

// ---- Scenario 8: prompt template rendering end-to-end ----
// Verifies the first-turn prompt contains the issue fields.

func TestE2E_PromptTemplateRendering(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Agent.MaxTurns = 1

	issue := domain.Issue{
		ID:          "prompt-1",
		Identifier:  "PROMPT-1",
		Title:       "Fix the bug",
		Description: "Details about the bug here.",
		State:       "In Progress",
	}
	tr := memory.New([]domain.Issue{issue})
	rt := mock.New(mock.MockOpts{
		RecordPrompts: true,
		Turns:         []mock.TurnScript{{Status: agent.TurnCompleted}},
	})

	wsMgr := workspace.NewManager(cfg)
	var logBuf bytes.Buffer
	log := observability.New(&logBuf)

	const tmpl = "You are working on {{ issue.identifier }}.\nTitle: {{ issue.title }}\nDesc: {{ issue.description }}"
	orch := orchestrator.New(cfg, tr, rt, wsMgr, log).WithPromptTemplate(tmpl)
	cancel, done := runOrch(t, orch)

	rec, ok := rt.(recorderI)
	if !ok {
		t.Fatal("mock does not implement recorderI")
	}
	waitFor(t, 5*time.Second, "turn ran", func() bool {
		return rec.RecordedTurnCount() >= 1
	})
	cancel()
	<-done

	prompts := rec.RecordedPrompts()
	if len(prompts) == 0 {
		t.Fatal("no prompts recorded")
	}
	p := prompts[0].Prompt
	for _, want := range []string{"PROMPT-1", "Fix the bug", "Details about the bug"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q, got: %q", want, p)
		}
	}
}
