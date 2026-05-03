// Package jira provides a JIRA Cloud REST v3 tracker adapter per SPEC §11.8.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultTimeout is the network timeout for JIRA REST API requests.
const defaultTimeout = 30 * time.Second

// Client is a thin HTTP client for the JIRA Cloud REST v3 API.
type Client struct {
	// Endpoint is the JIRA base URL (e.g. https://acme.atlassian.net).
	Endpoint string
	// Email is the JIRA user email address.
	Email string
	// APIToken is the JIRA API token.
	APIToken string
	// HTTP is the underlying HTTP client. If nil, a client with a 30 s timeout
	// is constructed on first use.
	HTTP *http.Client
}

// httpClient returns c.HTTP if set, otherwise constructs a default client.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// basicAuth returns the base64-encoded "email:token" for Basic authentication.
func (c *Client) basicAuth() string {
	creds := c.Email + ":" + c.APIToken
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// Do executes an HTTP request against the JIRA REST API.
//
//   - method is the HTTP method (GET, POST, etc.).
//   - path is the URL path (e.g. /rest/api/3/search).
//   - body is the request body (marshaled to JSON if non-nil).
//   - out is the target for unmarshaling the response JSON (if non-nil and response has a body).
//
// Non-2xx responses return a wrapped error with the status and up to 1 KB of the
// response body for debugging.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("jira: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	url := c.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", err)
	}

	req.Header.Set("Authorization", c.basicAuth())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("jira: http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Non-2xx: read up to 1 KB for debugging.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("jira: unexpected HTTP status %d: %s", resp.StatusCode, string(snippet))
	}

	// 204 No Content or no output target: nothing to decode.
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("jira: decode response: %w", err)
	}
	return nil
}
