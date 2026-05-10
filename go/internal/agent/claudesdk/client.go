package claudesdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiClient is a minimal Anthropic Messages API client wired to stdlib
// net/http. The full Go SDK pulls a deep dependency tree we do not need;
// the API surface we exercise (single endpoint, no streaming, no
// retries-beyond-context) fits in a few dozen lines.
type apiClient struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

func newAPIClient(apiKey, baseURL, model string, hc *http.Client) *apiClient {
	if hc == nil {
		hc = &http.Client{}
	}
	return &apiClient{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), model: model, http: hc}
}

// contentBlock represents one element in an Anthropic message content array.
// Only the fields Symphony uses are tagged; the API returns more (cache
// control, citations, …) which we ignore.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// message is the wire shape for both inbound and outbound messages.
type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// toolDef describes one tool to the Messages API.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// systemBlock matches the shape the Messages API expects when system is
// provided as an array (we use the simpler string variant).
type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
	Tools     []toolDef `json:"tools,omitempty"`
	System    string    `json:"system,omitempty"`
}

// messagesResponse is the subset of the Messages response Symphony reads.
type messagesResponse struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      tokenUsage     `json:"usage"`
}

type tokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Type  string   `json:"type"`
	Error apiError `json:"error"`
}

// createMessage posts one Messages API request and decodes the response.
func (c *apiClient) createMessage(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("claudesdk: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("claudesdk: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claudesdk: post messages: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("claudesdk: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env errorEnvelope
		if jerr := json.Unmarshal(rawBody, &env); jerr == nil && env.Error.Message != "" {
			return nil, fmt.Errorf("claudesdk: api error %s: %s", env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("claudesdk: api status %d: %s", resp.StatusCode, truncate(string(rawBody), 512))
	}

	var out messagesResponse
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, fmt.Errorf("claudesdk: decode response: %w", err)
	}
	return &out, nil
}

// truncate clips a string to n bytes with an ellipsis marker.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// defaultHTTPClient is the http.Client used when the runtime is built
// without a test-supplied override. The timeout is intentionally generous
// because the per-turn deadline is enforced by ctx.
var defaultHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
}
