// Package github provides a minimal GitHub REST client used by the PR
// reconciler to refresh PR state for issues whose agents have emitted a
// pr_link event. Stdlib net/http only — no SDK dep.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/openai/symphony/go/internal/domain"
)

// Client is a minimal GitHub REST client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client. token may be empty (unauthenticated requests subject
// to GitHub's anonymous rate limit).
func New(token string) *Client {
	return &Client{
		BaseURL: "https://api.github.com",
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// prResponse mirrors the relevant fields of GET /repos/{owner}/{repo}/pulls/{pull_number}.
type prResponse struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	HTMLURL  string     `json:"html_url"`
}

// GetPullRequest fetches the PR identified by owner/repo/number and returns
// the resolved domain.PullRequest. State is normalized to "open" / "closed"
// / "merged". The returned UpdatedAt is set to time.Now().
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (domain.PullRequest, error) {
	url := c.BaseURL + "/repos/" + owner + "/" + repo + "/pulls/" + strconv.Itoa(number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return domain.PullRequest{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return domain.PullRequest{}, fmt.Errorf("github: GET %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return domain.PullRequest{}, fmt.Errorf("github: GET %s: status %d", url, resp.StatusCode)
	}

	var body prResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return domain.PullRequest{}, fmt.Errorf("github: decode pr response: %w", err)
	}

	state := body.State
	if body.Merged {
		state = "merged"
	}
	url2 := body.HTMLURL
	if url2 == "" {
		url2 = "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(number)
	}
	return domain.PullRequest{
		URL:       url2,
		Number:    body.Number,
		Owner:     owner,
		Repo:      repo,
		State:     state,
		MergedAt:  body.MergedAt,
		Source:    "github_poll",
		UpdatedAt: time.Now().UTC(),
	}, nil
}
