// Package linear provides a Linear GraphQL tracker adapter per SPEC §11.2.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// defaultTimeout is the network timeout for Linear GraphQL requests per SPEC §11.2.
const defaultTimeout = 30 * time.Second

// Client is a thin GraphQL HTTP client for the Linear API.
// It is intentionally decoupled from Linear-specific query logic so that a
// similar shape could be reused by other GraphQL tracker adapters.
type Client struct {
	// Endpoint is the GraphQL API URL (e.g. https://api.linear.app/graphql).
	Endpoint string
	// APIKey is the Linear personal API key. Linear accepts keys raw — no
	// "Bearer" prefix — per https://developers.linear.app/docs/graphql/working-with-the-graphql-api.
	APIKey string
	// HTTP is the underlying HTTP client. If nil, a client with a 30 s timeout
	// is constructed on first use.
	HTTP *http.Client
}

// graphqlRequest is the JSON body sent to the GraphQL endpoint.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlResponse is the top-level GraphQL response envelope.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

// graphqlError is a single GraphQL error object.
type graphqlError struct {
	Message string `json:"message"`
}

// httpClient returns c.HTTP if set, otherwise constructs a default client with
// a 30 s timeout.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Do executes a GraphQL query/mutation against the configured endpoint.
//
// query is the raw GraphQL query string; vars is the variables map (may be
// nil); out is the target for unmarshaling the "data" field.
//
// Errors:
//   - Non-200 HTTP status → error with status code.
//   - GraphQL errors[] non-empty → wrapped error with first message + full payload.
//   - Missing "data" field → error.
//   - Unmarshal failures → wrapped error.
func (c *Client) Do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("linear: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("linear: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.APIKey)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("linear: http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear: unexpected HTTP status %d", resp.StatusCode)
	}

	var gqlResp graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gqlResp); err != nil {
		return fmt.Errorf("linear: decode response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		// Return a descriptive error with the first message plus the raw payload so
		// that callers can include it in logs/debug output.
		raw, _ := json.Marshal(gqlResp.Errors)
		return fmt.Errorf("linear: graphql error: %s (full errors: %s)", gqlResp.Errors[0].Message, raw)
	}

	if gqlResp.Data == nil {
		return fmt.Errorf("linear: response missing 'data' field")
	}

	if err := json.Unmarshal(gqlResp.Data, out); err != nil {
		return fmt.Errorf("linear: unmarshal data: %w", err)
	}
	return nil
}
