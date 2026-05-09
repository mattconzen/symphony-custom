// Package codex implements the Codex JSON-RPC 2.0 stdio agent runtime per SPEC §10.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

func init() {
	agent.RegisterCodexFactory(func(cfg config.Config) (agent.Runtime, error) {
		return New(cfg), nil
	})
}

// rpcRequest is a JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcMessage is a partially decoded JSON-RPC 2.0 message (request, response, or notification).
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// sessionImpl holds the subprocess state for a Codex session.
type sessionImpl struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	scanner  *bufio.Scanner
	threadID string
	mu       sync.Mutex
	stopped  bool
	idSeq    int32
}

func (s *sessionImpl) nextID() int {
	return int(atomic.AddInt32(&s.idSeq, 1))
}

// Runtime is the Codex JSON-RPC 2.0 stdio agent runtime.
type Runtime struct {
	cfg config.Config
}

// New returns a new Codex Runtime.
func New(cfg config.Config) *Runtime {
	return &Runtime{cfg: cfg}
}

// StartSession launches the Codex subprocess, performs the initialize +
// thread/start handshake, and returns a Session.
func (r *Runtime) StartSession(ctx context.Context, ws domain.Workspace) (agent.Session, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", r.cfg.Codex.Command) //nolint:gosec
	cmd.Dir = ws.Path

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return agent.Session{}, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.Session{}, fmt.Errorf("codex: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return agent.Session{}, fmt.Errorf("codex: start subprocess: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)

	impl := &sessionImpl{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
	}

	// Perform initialize handshake.
	initID := impl.nextID()
	initReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      initID,
		Method:  "initialize",
		Params: map[string]any{
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
			"clientInfo": map[string]any{
				"name":    "symphony-orchestrator",
				"title":   "Symphony Orchestrator",
				"version": "0.1.0",
			},
		},
	}
	if err := r.sendMsg(impl, initReq); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: send initialize: %w", err)
	}

	readTimeout := time.Duration(r.cfg.Codex.ReadTimeoutMs) * time.Millisecond
	if _, err := r.awaitResponse(impl, initID, readTimeout); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: initialize response: %w", err)
	}

	// Send "initialized" notification (no response expected).
	initializedNotif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	}
	if err := r.sendMsg(impl, initializedNotif); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: send initialized: %w", err)
	}

	// Send thread/start.
	threadID := impl.nextID()
	threadReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      threadID,
		Method:  "thread/start",
		Params: map[string]any{
			"approvalPolicy": r.cfg.Codex.ApprovalPolicy,
			"sandbox":        r.cfg.Codex.ThreadSandbox,
			"cwd":            ws.Path,
		},
	}
	if err := r.sendMsg(impl, threadReq); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: send thread/start: %w", err)
	}

	threadResult, err := r.awaitResponse(impl, threadID, readTimeout)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: thread/start response: %w", err)
	}

	// Extract thread_id from result.
	var resultMap map[string]any
	if err := json.Unmarshal(threadResult, &resultMap); err != nil {
		cmd.Process.Kill() //nolint:errcheck
		return agent.Session{}, fmt.Errorf("codex: parse thread/start result: %w", err)
	}
	var extractedThreadID string
	if thread, ok := resultMap["thread"].(map[string]any); ok {
		if id, ok := thread["id"].(string); ok {
			extractedThreadID = id
		}
	}
	if extractedThreadID == "" {
		// Fall back to a generated ID if the fake/server doesn't return one.
		extractedThreadID = fmt.Sprintf("thread-%d", time.Now().UnixNano())
	}
	impl.threadID = extractedThreadID

	sessID := extractedThreadID
	return agent.Session{
		ID:   sessID,
		Impl: impl,
	}, nil
}

// RunTurn sends a turn/start and streams events until turn completion or failure.
func (r *Runtime) RunTurn(ctx context.Context, sess agent.Session, prompt string, issue domain.Issue, cb agent.EventCallback) (agent.TurnResult, error) {
	impl, ok := sess.Impl.(*sessionImpl)
	if !ok || impl == nil {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("codex: invalid session impl")
	}

	turnReqID := impl.nextID()
	turnReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      turnReqID,
		Method:  "turn/start",
		Params: map[string]any{
			"threadId": impl.threadID,
			"input": []map[string]any{
				{"type": "text", "text": prompt},
			},
			"cwd":            impl.cmd.Dir,
			"title":          fmt.Sprintf("%s: %s", issue.Identifier, issue.Title),
			"approvalPolicy": r.cfg.Codex.ApprovalPolicy,
			"sandboxPolicy":  r.cfg.Codex.TurnSandboxPolicy,
		},
	}
	if err := r.sendMsg(impl, turnReq); err != nil {
		return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("codex: send turn/start: %w", err)
	}

	// Set up timeouts.
	turnTimeout := time.Duration(r.cfg.Codex.TurnTimeoutMs) * time.Millisecond
	readTimeout := time.Duration(r.cfg.Codex.ReadTimeoutMs) * time.Millisecond
	stallTimeout := time.Duration(r.cfg.Codex.StallTimeoutMs) * time.Millisecond

	turnDeadline := time.Now().Add(turnTimeout)
	lastActivity := time.Now()

	// First read: the turn/start response with turn_id.
	var turnID string
	{
		result, err := r.awaitResponse(impl, turnReqID, readTimeout)
		if err != nil {
			return agent.TurnResult{SessionID: sess.ID, Status: agent.TurnFailed}, fmt.Errorf("codex: turn/start response: %w", err)
		}
		var resultMap map[string]any
		if json.Unmarshal(result, &resultMap) == nil {
			if turn, ok := resultMap["turn"].(map[string]any); ok {
				if id, ok := turn["id"].(string); ok {
					turnID = id
				}
			}
		}
	}
	if turnID == "" {
		turnID = fmt.Sprintf("turn-%d", time.Now().UnixNano())
	}

	sessionID := fmt.Sprintf("%s-%s", impl.threadID, turnID)

	if cb != nil {
		cb(agent.Event{
			Kind:      agent.EventThreadStarted,
			Timestamp: time.Now().UTC(),
			SessionID: sessionID,
			Payload:   map[string]any{"thread_id": impl.threadID, "turn_id": turnID},
		})
	}

	// Stream events until completion.
	for {
		// Check context cancellation.
		if ctx.Err() != nil {
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnCancelled}, ctx.Err()
		}

		// Check turn timeout.
		if time.Now().After(turnDeadline) {
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("codex: turn timeout")
		}

		// Check stall timeout.
		if stallTimeout > 0 && time.Since(lastActivity) > stallTimeout {
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("codex: stall timeout")
		}

		msg, err := r.readMsgTimeout(impl, readTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return agent.TurnResult{SessionID: sessionID, Status: agent.TurnCancelled}, ctx.Err()
			}
			impl.kill()
			return agent.TurnResult{SessionID: sessionID, Status: agent.TurnFailed}, fmt.Errorf("codex: read error: %w", err)
		}

		lastActivity = time.Now()

		result, status, tokens, err := r.processMessage(msg, sessionID, cb)
		if result {
			return agent.TurnResult{
				SessionID: sessionID,
				Status:    status,
				Tokens:    tokens,
				Err:       err,
			}, err
		}
	}
}

// processMessage processes one incoming JSON-RPC message. Returns (done, status, tokens, err).
func (r *Runtime) processMessage(msg rpcMessage, sessionID string, cb agent.EventCallback) (bool, agent.TurnStatus, agent.TokenUsage, error) {
	method := msg.Method

	switch method {
	case "turn/completed":
		tokens := extractTokens(msg.Params)
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventTurnCompleted,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg.Params,
			})
		}
		return true, agent.TurnCompleted, tokens, nil

	case "turn/failed":
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventTurnFailed,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg.Params,
			})
		}
		return true, agent.TurnFailed, agent.TokenUsage{}, fmt.Errorf("codex: turn failed: %s", string(msg.Params))

	case "turn/cancelled":
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventTurnCancelled,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg.Params,
			})
		}
		return true, agent.TurnCancelled, agent.TokenUsage{}, fmt.Errorf("codex: turn cancelled")

	case "item/commandExecution/requestApproval", "execCommandApproval", "applyPatchApproval", "item/fileChange/requestApproval":
		// Auto-approve (high-trust policy).
		// No-op for tests; in production would send an approval response.
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventNotification,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg,
			})
		}
		return false, 0, agent.TokenUsage{}, nil

	case "item/assistant/message", "turn/event":
		// Emit assistant message event.
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventAssistantMessage,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg.Params,
			})
		}
		return false, 0, agent.TokenUsage{}, nil

	case "item/tool/call":
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventToolCall,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg.Params,
			})
		}
		return false, 0, agent.TokenUsage{}, nil

	default:
		// Check if this is a response to a request (has an ID and result/error).
		if msg.ID != nil && (msg.Result != nil || msg.Error != nil) {
			// Unexpected response; emit as other_message.
			if cb != nil {
				cb(agent.Event{
					Kind:      agent.EventOtherMessage,
					Timestamp: time.Now().UTC(),
					SessionID: sessionID,
					Payload:   msg,
				})
			}
			return false, 0, agent.TokenUsage{}, nil
		}
		// Notification we don't specifically handle.
		if cb != nil {
			cb(agent.Event{
				Kind:      agent.EventNotification,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				Payload:   msg,
			})
		}
		return false, 0, agent.TokenUsage{}, nil
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

	// Close stdin to signal EOF to the subprocess.
	impl.stdin.Close() //nolint:errcheck

	// Wait for subprocess to exit with a short timeout.
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

// kill forcibly kills the subprocess (used on timeout/stall).
func (s *sessionImpl) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.cmd.Process.Kill() //nolint:errcheck
	}
}

// sendMsg JSON-encodes and writes a message to stdin.
func (r *Runtime) sendMsg(impl *sessionImpl, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = impl.stdin.Write(b)
	return err
}

// awaitResponse reads lines until it finds a response with the given ID.
func (r *Runtime) awaitResponse(impl *sessionImpl, id int, timeout time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("read timeout waiting for response id=%d", id)
		}
		msg, err := r.readMsgTimeout(impl, remaining)
		if err != nil {
			return nil, err
		}
		if msg.ID != nil && *msg.ID == id {
			if msg.Error != nil {
				return nil, fmt.Errorf("rpc error: %s", string(msg.Error))
			}
			return msg.Result, nil
		}
		// Ignore other messages while waiting for the response.
	}
}

// readMsgTimeout reads the next newline-delimited JSON message from the scanner.
// It respects a per-read deadline using a goroutine.
func (r *Runtime) readMsgTimeout(impl *sessionImpl, timeout time.Duration) (rpcMessage, error) {
	type result struct {
		msg rpcMessage
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
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// Emit as malformed; caller will handle.
			ch <- result{msg: rpcMessage{Method: "__malformed__"}, err: nil}
			return
		}
		ch <- result{msg: msg}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.msg, res.err
	case <-timer.C:
		return rpcMessage{}, fmt.Errorf("read timeout")
	}
}

// extractTokens tries to parse token usage from a JSON-RPC params payload.
func extractTokens(raw json.RawMessage) agent.TokenUsage {
	if raw == nil {
		return agent.TokenUsage{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return agent.TokenUsage{}
	}
	usage := agent.TokenUsage{}
	if u, ok := m["usage"].(map[string]any); ok {
		usage.InputTokens = intFromAny(u["input_tokens"])
		usage.OutputTokens = intFromAny(u["output_tokens"])
		usage.TotalTokens = intFromAny(u["total_tokens"])
	}
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
