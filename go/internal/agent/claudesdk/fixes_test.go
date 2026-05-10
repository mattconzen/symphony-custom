package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// --- T5: zero TurnTimeoutMs gets a sensible default in New ----------------

func TestT5_ZeroTurnTimeoutMsDefaultedInNew(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{{
			Content:    []contentBlock{{Type: "text", Text: "done"}},
			StopReason: "end_turn",
		}},
	}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	// TurnTimeoutMs intentionally 0; without the defensive default,
	// context.WithTimeout(ctx, 0) cancels immediately and the turn would fail.
	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 0, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status, "expected TurnCompleted, not TurnCancelled, with zero TurnTimeoutMs")
}

// --- T6: stop_reason=max_tokens surfaces as TurnFailed --------------------

func TestT6_MaxTokensStopReasonFailsTurn(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{{
			Content:    []contentBlock{{Type: "text", Text: "partial"}},
			StopReason: "max_tokens",
			Usage:      tokenUsage{InputTokens: 10, OutputTokens: 4},
		}},
	}
	rt, _ := newTestRuntime(t, api)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	var seen []agent.Event
	cb := func(ev agent.Event) { seen = append(seen, ev) }

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, cb)
	require.Error(t, err)
	assert.Equal(t, agent.TurnFailed, res.Status)
	assert.Contains(t, err.Error(), "max_tokens_truncation")

	var sawFailEvent bool
	for _, ev := range seen {
		if ev.Kind == agent.EventTurnFailed {
			if p, ok := ev.Payload.(map[string]any); ok {
				if reason, _ := p["reason"].(string); reason == "max_tokens_truncation" {
					sawFailEvent = true
				}
			}
		}
	}
	assert.True(t, sawFailEvent, "expected EventTurnFailed with reason=max_tokens_truncation")
}

// --- T7: prompt caching shape on system and last content block -----------

func TestT7_PromptCachingShape(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{{
			Content:    []contentBlock{{Type: "text", Text: "ok"}},
			StopReason: "end_turn",
			Usage:      tokenUsage{InputTokens: 5, OutputTokens: 2, CacheCreationInputTokens: 100, CacheReadInputTokens: 50},
		}},
	}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 5_000, MaxIterations: 5, SystemPrompt: "you are a test"},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	var seen []agent.Event
	res, err := rt.RunTurn(context.Background(), sess, "hello", domain.Issue{ID: "1"}, func(ev agent.Event) { seen = append(seen, ev) })
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)

	require.Len(t, api.captured, 1)
	req := api.captured[0]

	// (a) System block carries cache_control.
	require.Len(t, req.System, 1)
	assert.Equal(t, "text", req.System[0].Type)
	assert.Equal(t, "you are a test", req.System[0].Text)
	require.NotNil(t, req.System[0].CacheControl)
	assert.Equal(t, "ephemeral", req.System[0].CacheControl.Type)

	// (b) Last content block of the last user message carries cache_control.
	require.NotEmpty(t, req.Messages)
	lastMsg := req.Messages[len(req.Messages)-1]
	assert.Equal(t, "user", lastMsg.Role)
	require.NotEmpty(t, lastMsg.Content)
	lastBlk := lastMsg.Content[len(lastMsg.Content)-1]
	require.NotNil(t, lastBlk.CacheControl, "expected cache_control on final content block")
	assert.Equal(t, "ephemeral", lastBlk.CacheControl.Type)

	// (c) Cache token usage propagates to EventTurnCompleted payload.
	var saw bool
	for _, ev := range seen {
		if ev.Kind == agent.EventTurnCompleted {
			p, _ := ev.Payload.(map[string]any)
			if cc, _ := p["cache_creation_input_tokens"].(int); cc == 100 {
				if cr, _ := p["cache_read_input_tokens"].(int); cr == 50 {
					saw = true
				}
			}
		}
	}
	assert.True(t, saw, "expected cache_creation_input_tokens/cache_read_input_tokens in EventTurnCompleted payload")
}

func TestT7_AnthropicVersionHeader(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResponse{
			Content:    []contentBlock{{Type: "text", Text: "ok"}},
			StopReason: "end_turn",
		})
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 5_000, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	_, err = rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "2024-10-22", captured)
}

// --- T8: doublestar glob -------------------------------------------------

func TestT8_GlobDoublestar_MatchesRootAndNested(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "root.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "shallow.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "a", "b", "deep.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "a", "b", "other.md"), []byte("md"), 0o644))

	out, err := globTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"**/*.go"}`))
	require.NoError(t, err)
	assert.Contains(t, out, "root.go")
	assert.Contains(t, out, filepath.Join("src", "shallow.go"))
	assert.Contains(t, out, filepath.Join("src", "a", "b", "deep.go"))
	assert.NotContains(t, out, "other.md")
}

func TestT8_GlobDoublestar_PrefixedWildcard(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "a", "b"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "foo.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "a", "b", "foo.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "elsewhere", "foo.go"), []byte("//"), 0o644))

	out, err := globTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"src/**/foo.go"}`))
	require.NoError(t, err)
	assert.Contains(t, out, filepath.Join("src", "foo.go"))
	assert.Contains(t, out, filepath.Join("src", "a", "b", "foo.go"))
	assert.NotContains(t, out, filepath.Join("elsewhere", "foo.go"))
}

func TestT8_GlobPlainStar(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("//"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.go"), []byte("//"), 0o644))

	out, err := globTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"*.go"}`))
	require.NoError(t, err)
	// Plain *.go must match root-level .go files (basename fallback) but not
	// the deep ones (no /).
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "b.go")
	// The basename fallback may also surface nested *.go; the test asserts the
	// roots are present. Doublestar is not in this pattern.
}

func TestT8_GlobLiteralPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x", "y.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x", "z.txt"), []byte("hi"), 0o644))

	out, err := globTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"x/y.txt"}`))
	require.NoError(t, err)
	assert.Contains(t, out, filepath.Join("x", "y.txt"))
	assert.NotContains(t, out, filepath.Join("x", "z.txt"))
}

// --- T9: stream-bound readTool truncation --------------------------------

func TestT9_ReadToolTruncationByStream(t *testing.T) {
	// Lower the cap so the test stays small. The +"\n…[truncated]\n" marker
	// is appended when the file exceeds readMaxBytes.
	orig := readMaxBytes
	readMaxBytes = 1024
	t.Cleanup(func() { readMaxBytes = orig })

	dir := t.TempDir()
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644))

	out, err := readTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"big.txt"}`))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, "\n…[truncated]\n"), "expected truncation marker, got tail %q", tail(out, 32))
	// The body before the marker should be exactly readMaxBytes bytes.
	prefix := strings.TrimSuffix(out, "\n…[truncated]\n")
	assert.Equal(t, readMaxBytes, len(prefix))
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// --- T10: API response read limit ----------------------------------------

func TestT10_ResponseUnder32MiBSucceeds(t *testing.T) {
	// 8 MiB of padding inside a valid messages response. The handler writes
	// the JSON directly so we control the body size precisely.
	padding := strings.Repeat("p", 8*1024*1024)
	body := fmt.Sprintf(`{"id":"m","role":"assistant","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, padding)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 30_000, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)
}

func TestT10_ResponseOver32MiBSurfacesClearError(t *testing.T) {
	// Stream more than 32 MiB. We don't have to construct valid JSON; the
	// LimitReader will hit the cap before any decode runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("x", 1024*1024)
		for i := 0; i < 33; i++ {
			_, _ = io.WriteString(w, chunk)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 30_000, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	require.Error(t, err)
	assert.Equal(t, agent.TurnFailed, res.Status)
	assert.Contains(t, err.Error(), "response exceeded 32 MiB limit")
}

// --- T11: 429 retry with Retry-After -------------------------------------

func TestT11_429WithRetryAfterEventuallySucceeds(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"type":"error","error":{"type":"rate_limit","message":"slow down"}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResponse{
			Content:    []contentBlock{{Type: "text", Text: "ok"}},
			StopReason: "end_turn",
		})
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 30_000, MaxIterations: 5},
	}
	rt, err := New(cfg, srv.Client())
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	start := time.Now()
	res, err := rt.RunTurn(context.Background(), sess, "hi", domain.Issue{ID: "1"}, nil)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)
	assert.Equal(t, int32(3), atomic.LoadInt32(&count), "expected 2 retries + 1 success = 3 attempts")
	assert.Less(t, elapsed, 5*time.Second, "retry loop should complete in <5s, took %s", elapsed)
}

// --- T12: empty tool output substituted with "(no output)" ---------------

type emptyTool struct{}

func (emptyTool) Name() string                { return "Empty" }
func (emptyTool) Description() string         { return "returns empty" }
func (emptyTool) Schema() map[string]any      { return map[string]any{"type": "object", "properties": map[string]any{}} }
func (emptyTool) Run(_ context.Context, _ string, _ json.RawMessage) (string, error) {
	return "", nil
}

func TestT12_EmptyToolResultBecomesPlaceholder(t *testing.T) {
	api := &fakeAPI{
		responses: []messagesResponse{
			{Content: []contentBlock{
				{Type: "tool_use", ID: "tu_1", Name: "Empty", Input: json.RawMessage(`{}`)},
			}, StopReason: "tool_use"},
			{Content: []contentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn"},
		},
	}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	cfg := config.Config{
		Agent:     config.Agent{Runtime: "claude_sdk"},
		ClaudeSDK: config.ClaudeSDK{Model: "claude-test", APIKey: "k", APIKeyEnv: "ANTHROPIC_API_KEY", BaseURL: srv.URL, MaxTokens: 256, TurnTimeoutMs: 5_000, MaxIterations: 5},
	}
	rt, err := NewWithTools(cfg, srv.Client(), emptyTool{})
	require.NoError(t, err)

	ws := domain.Workspace{Path: t.TempDir(), Key: "ws"}
	sess, err := rt.StartSession(context.Background(), ws)
	require.NoError(t, err)

	res, err := rt.RunTurn(context.Background(), sess, "go", domain.Issue{ID: "1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, agent.TurnCompleted, res.Status)

	require.Len(t, api.captured, 2, "expected 2 API requests")
	second := api.captured[1]
	// The new user message (tool_result block) is the last entry.
	last := second.Messages[len(second.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.NotEmpty(t, last.Content)
	tr := last.Content[len(last.Content)-1]
	assert.Equal(t, "tool_result", tr.Type)
	assert.Equal(t, "(no output)", tr.Content)
}

// --- T13: rune-aware grep clipping --------------------------------------

func TestT13_GrepClippingIsRuneAware(t *testing.T) {
	dir := t.TempDir()
	// Build a line whose multibyte rune (日, 3 bytes) lands across the 200-byte
	// boundary so a byte slice would split it.
	prefix := strings.Repeat("a", 198) // 198 bytes
	line := prefix + "日本語MATCH"      // 198 + 9 + 5 = 212 bytes, 198 + 3 + 5 = 206 runes
	require.NoError(t, os.WriteFile(filepath.Join(dir, "u.txt"), []byte(line+"\n"), 0o644))

	out, err := grepTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"MATCH"}`))
	require.NoError(t, err)
	// Strip the "u.txt:1:" prefix to look at the clipped line.
	parts := strings.SplitN(out, ":", 3)
	require.Len(t, parts, 3)
	clipped := parts[2]
	clipped = strings.TrimSuffix(clipped, "\n")
	assert.True(t, utf8.ValidString(clipped), "clipped line should be valid UTF-8")
	// At most 200 runes, plus a single trailing "…" marker when truncation happened.
	clippedNoEllipsis := strings.TrimSuffix(clipped, "…")
	assert.LessOrEqual(t, utf8.RuneCountInString(clippedNoEllipsis), 200)
}
