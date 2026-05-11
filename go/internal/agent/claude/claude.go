// Package claude implements the Claude stream-json stdio agent runtime per SPEC §10.8.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

func init() {
	agent.RegisterClaudeFactory(func(cfg config.Config) (agent.Runtime, error) {
		return New(cfg), nil
	})
}

// claudeEvent is a partially decoded Claude stream-json event.
type claudeEvent struct {
	Type      string          `json:"type"`
	SubType   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Result    string          `json:"result,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
}

// sessionImpl holds the subprocess state for a Claude session.
type sessionImpl struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex
	stopped bool
}

// Runtime is the Claude stream-json stdio agent runtime.
type Runtime struct {
	cfg config.Config
}

// New returns a new Claude Runtime.
func New(cfg config.Config) *Runtime {
	return &Runtime{cfg: cfg}
}

// StartSession launches the Claude subprocess. No handshake is required;
// Claude is ready to receive input immediately.
func (r *Runtime) StartSession(ctx context.Context, ws domain.Workspace) (agent.Session, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", r.cfg.Claude.Command) //nolint:gosec
	cmd.Dir = ws.Path

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return agent.Session{}, fmt.Errorf("claude: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.Session{}, fmt.Errorf("claude: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return agent.Session{}, fmt.Errorf("claude: start subprocess: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)

	impl := &sessionImpl{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
	}

	// Generate a session ID; the real session_id will come from the system/init event.
	sessID := fmt.Sprintf("claude-%x", time.Now().UnixNano())

	return agent.Session{
		ID:   sessID,
		Impl: impl,
	}, nil
}

// RunTurn writes the prompt to stdin, closes stdin (EOF), and streams events
// from stdout until a result event.
func (r *Runtime) RunTurn(ctx context.Context, sess agent.Session, prompt string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	impl, ok := sess.Impl.(*sessionImpl)
	if !ok || impl == nil {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("claude: invalid session impl")
	}

	if err := r.writePrompt(impl, prompt); err != nil {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, err
	}

	readTimeout := time.Duration(r.cfg.Claude.ReadTimeoutMs) * time.Millisecond
	stallTimeout := time.Duration(r.cfg.Claude.StallTimeoutMs) * time.Millisecond
	turnDeadline := time.Now().Add(time.Duration(r.cfg.Claude.TurnTimeoutMs) * time.Millisecond)
	lastActivity := time.Now()
	sessionID := sess.ID

	for {
		if ctx.Err() != nil {
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnCancelled}, ctx.Err()
		}
		if time.Now().After(turnDeadline) {
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("claude: turn timeout")
		}
		if stallTimeout > 0 && time.Since(lastActivity) > stallTimeout {
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("claude: stall timeout")
		}

		ev, err := r.readEventTimeout(impl, readTimeout)
		if err != nil {
			if ctx.Err() != nil {
				impl.kill()
				return agent.TurnResult{SessionID: sessionID, Status: agent.TurnCancelled}, ctx.Err()
			}
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("claude: read error: %w", err)
		}

		lastActivity = time.Now()

		done, res := r.dispatchEvent(ev, &sessionID, cb)
		if done {
			return res, res.Err
		}
	}
}

// writePrompt sends the prompt JSON line to stdin and signals EOF per SPEC §10.8.2.
func (r *Runtime) writePrompt(impl *sessionImpl, prompt string) error {
	promptMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": prompt},
			},
		},
	}
	b, err := json.Marshal(promptMsg)
	if err != nil {
		return fmt.Errorf("claude: marshal prompt: %w", err)
	}
	b = append(b, '\n')
	if _, err := impl.stdin.Write(b); err != nil {
		return fmt.Errorf("claude: write prompt: %w", err)
	}
	impl.stdin.Close() //nolint:errcheck
	return nil
}

// dispatchEvent maps one Claude event to a Symphony event and emits it. Returns
// (true, result) when the turn is complete (success or failure).
func (r *Runtime) dispatchEvent(ev claudeEvent, sessionID *string, cb agent.EventCallback) (bool, agent.TurnResult) {
	emitPayload := func(kind agent.EventKind, payload any) {
		if cb != nil {
			cb(agent.Event{
				Kind:      kind,
				Timestamp: time.Now().UTC(),
				SessionID: *sessionID,
				Payload:   payload,
			})
		}
	}

	switch ev.Type {
	case "system":
		return r.handleSystem(ev, sessionID, cb)
	case "assistant":
		// Surface assistant text as a plain string payload, mirroring the
		// claude_sdk runtime. Consumers (e.g. spec-gen) rely on this contract
		// to render Markdown rather than printing the raw event struct.
		emitPayload(agent.EventAssistantMessage, extractAssistantText(ev.Message))
	case "user":
		emitPayload(agent.EventToolResult, ev)
	case "result":
		return r.handleResult(ev, *sessionID, cb)
	case "__malformed__":
		emitPayload(agent.EventMalformed, ev)
	default:
		emitPayload(agent.EventOtherMessage, ev)
	}
	return false, agent.TurnResult{}
}

// extractAssistantText pulls the concatenated text from an assistant message's
// content blocks. Returns "" if the message is empty or unparseable.
func extractAssistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var parts []string
	for _, blk := range msg.Content {
		if blk.Type == "text" && blk.Text != "" {
			parts = append(parts, blk.Text)
		}
	}
	return strings.Join(parts, "")
}

// handleSystem processes system events, updating the session ID on init.
func (r *Runtime) handleSystem(ev claudeEvent, sessionID *string, cb agent.EventCallback) (bool, agent.TurnResult) {
	if ev.SubType == "init" && ev.SessionID != "" {
		*sessionID = ev.SessionID
	}
	kind := agent.EventOtherMessage
	if ev.SubType == "init" {
		kind = agent.EventThreadStarted
	}
	if cb != nil {
		cb(agent.Event{
			Kind:      kind,
			Timestamp: time.Now().UTC(),
			SessionID: *sessionID,
			Payload:   ev,
		})
	}
	return false, agent.TurnResult{}
}

// handleResult processes result events, returning a completed or failed TurnResult.
func (r *Runtime) handleResult(ev claudeEvent, sessionID string, cb agent.EventCallback) (bool, agent.TurnResult) {
	tokens := extractClaudeTokens(ev.Usage)
	switch {
	case ev.SubType == "success":
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventTurnCompleted,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   ev,
			})
		}
		return true, agent.TurnResult{SessionID: sessionID, Status: agent.TurnCompleted, Tokens: tokens}
	case strings.HasPrefix(ev.SubType, "error_"):
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventTurnFailed,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   ev,
			})
		}
		turnErr := fmt.Errorf("%s", ev.SubType) //nolint:err113
		return true, agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed, Tokens: tokens, Err: turnErr}
	default:
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventOtherMessage,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   ev,
			})
		}
		return false, agent.TurnResult{}
	}
}

// StopSession closes stdin and waits for the subprocess to exit.
func (r *Runtime) StopSession(_ context.Context, sess agent.Session) error {
	impl, ok := sess.Impl.(*sessionImpl)
	if !ok || impl == nil {
		return nil
	}
	impl.mu.Lock()
	if impl.stopped {
		impl.mu.Unlock()
		return nil
	}
	impl.stopped = true
	impl.mu.Unlock()

	impl.stdin.Close() //nolint:errcheck

	done := make(chan error, 1)
	go func() { done <- impl.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		impl.cmd.Process.Kill() //nolint:errcheck
		<-done
	}
	return nil
}

// kill forcibly kills the subprocess.
func (s *sessionImpl) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.cmd.Process.Kill() //nolint:errcheck
	}
}

// readEventTimeout reads and parses one JSON event line from stdout.
func (r *Runtime) readEventTimeout(impl *sessionImpl, timeout time.Duration) (claudeEvent, error) {
	type result struct {
		ev  claudeEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if !impl.scanner.Scan() {
			err := impl.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			ch <- result{err: err}
			return
		}
		line := impl.scanner.Bytes()
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Emit as malformed.
			ch <- result{ev: claudeEvent{Type: "__malformed__"}}
			return
		}
		ch <- result{ev: ev}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.ev, res.err
	case <-timer.C:
		return claudeEvent{}, fmt.Errorf("read timeout")
	}
}

// extractClaudeTokens parses the usage field from a Claude result event.
func extractClaudeTokens(raw json.RawMessage) agent.TokenUsage {
	if raw == nil {
		return agent.TokenUsage{}
	}
	var u map[string]any
	if err := json.Unmarshal(raw, &u); err != nil {
		return agent.TokenUsage{}
	}
	usage := agent.TokenUsage{}
	usage.InputTokens = intFromAny(u["input_tokens"])
	usage.OutputTokens = intFromAny(u["output_tokens"])
	usage.TotalTokens = intFromAny(u["total_tokens"])
	if usage.TotalTokens == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
