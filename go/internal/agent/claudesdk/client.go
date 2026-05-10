package claudesdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// cacheControl marks a content block as a prompt-caching breakpoint.
// Anthropic permits up to 4 ephemeral breakpoints per request.
type cacheControl struct {
	Type string `json:"type"`
}

// contentBlock represents one element in an Anthropic message content array.
// Only the fields Symphony uses are tagged; the API returns more (citations, …)
// which we ignore.
type contentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      any             `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// systemTextBlock is the array-form system prompt the API accepts. We use the
// array form (not the string form) so we can attach cache_control.
type systemTextBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
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

// messagesRequest is the wire shape Symphony serialises. System is the
// array form so we can attach a cache_control breakpoint.
type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Messages  []message         `json:"messages"`
	Tools     []toolDef         `json:"tools,omitempty"`
	System    []systemTextBlock `json:"system,omitempty"`
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
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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
// 429s and 5xx responses trigger a bounded retry loop (max 3 attempts, capped
// at 30s total wall-clock) that honors a server-supplied Retry-After header
// when present.
func (c *apiClient) createMessage(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("claudesdk: marshal request: %w", err)
	}

	const maxAttempts = 3
	const totalBudget = 30 * time.Second
	start := timeNow()
	backoff := time.Second

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, retryAfter, retryable, err := c.doOnce(ctx, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable || attempt == maxAttempts-1 {
			return nil, err
		}
		wait := backoff
		if retryAfter > 0 {
			wait = retryAfter
		}
		if elapsed := timeNow().Sub(start); elapsed+wait > totalBudget {
			return nil, err
		}
		// If the caller's context will expire before we could plausibly retry,
		// surface the original error rather than the (less informative)
		// context.DeadlineExceeded.
		if dl, ok := ctx.Deadline(); ok && timeNow().Add(wait).After(dl) {
			return nil, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
	}
	return nil, lastErr
}

// timeNow is overridable in tests to dodge real wall-clock waits when needed.
var timeNow = time.Now

// doOnce performs a single HTTP exchange. retryable is true when the caller
// should sleep and try again (429 or 5xx). retryAfter conveys the parsed
// Retry-After header, or zero when absent.
func (c *apiClient) doOnce(ctx context.Context, body []byte) (resp *messagesResponse, retryAfter time.Duration, retryable bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, 0, false, fmt.Errorf("claudesdk: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2024-10-22")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, false, fmt.Errorf("claudesdk: post messages: %w", err)
	}
	defer httpResp.Body.Close() //nolint:errcheck

	rawBody, err := io.ReadAll(io.LimitReader(httpResp.Body, apiResponseMaxBytes+1))
	if err != nil {
		return nil, 0, false, fmt.Errorf("claudesdk: read response: %w", err)
	}
	if int64(len(rawBody)) > apiResponseMaxBytes {
		return nil, 0, false, fmt.Errorf("claudesdk: response exceeded %d MiB limit", apiResponseMaxBytes/(1024*1024))
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		var apiErrMsg string
		var env errorEnvelope
		if jerr := json.Unmarshal(rawBody, &env); jerr == nil && env.Error.Message != "" {
			apiErrMsg = fmt.Sprintf("claudesdk: api error %s: %s", env.Error.Type, env.Error.Message)
		} else {
			apiErrMsg = fmt.Sprintf("claudesdk: api status %d: %s", httpResp.StatusCode, truncate(string(rawBody), 512))
		}
		isRetryable := httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500
		ra := parseRetryAfter(httpResp.Header.Get("Retry-After"))
		return nil, ra, isRetryable, fmt.Errorf("%s", apiErrMsg)
	}

	var out messagesResponse
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, 0, false, fmt.Errorf("claudesdk: decode response: %w", err)
	}
	return &out, 0, false, nil
}

// parseRetryAfter accepts either an integer seconds value or an HTTP-date.
// Returns zero when the header is absent or malformed.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
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

// apiResponseMaxBytes caps the response body Symphony will read. Anthropic
// max-tokens responses with embedded tool inputs comfortably exceed 4 MiB; the
// prior 4 MiB cut surfaced as an "unexpected end of JSON input" error. 32 MiB
// is roomy enough for any plausible Messages API response.
const apiResponseMaxBytes int64 = 32 * 1024 * 1024
