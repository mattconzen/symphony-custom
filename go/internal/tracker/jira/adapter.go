package jira

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// maxResults is the maximum number of issues returned per search request.
const maxResults = 50

// Adapter implements tracker.Tracker for JIRA Cloud REST v3.
type Adapter struct {
	cfg    config.Config
	client *Client
}

// New creates a new JIRA Adapter. httpClient may be nil; a default 30 s client
// will be used. Returns an error if required fields (Endpoint, Email, APIToken,
// ProjectKey) are missing.
func New(cfg config.Config, httpClient *http.Client) (*Adapter, error) {
	j := cfg.Tracker.JIRA
	if j.Endpoint == "" {
		return nil, fmt.Errorf("jira: tracker.jira.endpoint is required")
	}
	if j.Email == "" {
		return nil, fmt.Errorf("jira: tracker.jira.email is required")
	}
	if j.APIToken == "" {
		return nil, fmt.Errorf("jira: tracker.jira.api_token is required")
	}
	if j.ProjectKey == "" {
		return nil, fmt.Errorf("jira: tracker.jira.project_key is required")
	}

	c := &Client{
		Endpoint: j.Endpoint,
		Email:    j.Email,
		APIToken: j.APIToken,
		HTTP:     httpClient,
	}
	return &Adapter{cfg: cfg, client: c}, nil
}

// FetchCandidateIssues returns issues in the configured active states.
// Uses JQLOverride verbatim when set; otherwise constructs default JQL.
func (a *Adapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	var jql string
	if a.cfg.Tracker.JIRA.JQLOverride != "" {
		jql = a.cfg.Tracker.JIRA.JQLOverride
	} else {
		jql = a.buildStateJQL(a.cfg.Tracker.ActiveStates)
	}
	return a.search(ctx, jql)
}

// FetchIssuesByStates returns issues in the given states.
// JQLOverride is NOT applied here; the caller-supplied states are always used.
func (a *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}
	jql := a.buildStateJQL(states)
	return a.search(ctx, jql)
}

// FetchIssueStatesByIDs returns issues by their JIRA keys (identifiers).
// In JIRA Cloud, identifiers are keys (e.g. MT-1); numeric IDs are not used here.
func (a *Adapter) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Build: key in ("MT-1","MT-2")
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = `"` + id + `"`
	}
	jql := "key in (" + strings.Join(quoted, ",") + ")"
	return a.search(ctx, jql)
}

// CreateComment appends a comment (as ADF) to the given issue key.
func (a *Adapter) CreateComment(ctx context.Context, issueKey, body string) error {
	reqBody := map[string]any{
		"body": TextToADF(body),
	}
	path := "/rest/api/3/issue/" + issueKey + "/comment"
	return a.client.Do(ctx, http.MethodPost, path, reqBody, nil)
}

// UpdateIssueState transitions the issue to the named state via JIRA transitions.
// Step 1: GET /rest/api/3/issue/{key}/transitions to find the transition ID.
// Step 2: POST /rest/api/3/issue/{key}/transitions with the found transition ID.
func (a *Adapter) UpdateIssueState(ctx context.Context, issueKey, stateName string) error {
	// Step 1: fetch available transitions.
	var transResp struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	getPath := "/rest/api/3/issue/" + issueKey + "/transitions"
	if err := a.client.Do(ctx, http.MethodGet, getPath, nil, &transResp); err != nil {
		return fmt.Errorf("jira: fetch transitions for %s: %w", issueKey, err)
	}

	// Find matching transition (case-insensitive on to.name or name).
	lowerTarget := strings.ToLower(stateName)
	var transID string
	for _, t := range transResp.Transitions {
		if strings.ToLower(t.To.Name) == lowerTarget || strings.ToLower(t.Name) == lowerTarget {
			transID = t.ID
			break
		}
	}
	if transID == "" {
		return fmt.Errorf("jira: transition to state %q not found for issue %s", stateName, issueKey)
	}

	// Step 2: apply the transition.
	postBody := map[string]any{
		"transition": map[string]any{
			"id": transID,
		},
	}
	postPath := "/rest/api/3/issue/" + issueKey + "/transitions"
	if err := a.client.Do(ctx, http.MethodPost, postPath, postBody, nil); err != nil {
		return fmt.Errorf("jira: apply transition for %s: %w", issueKey, err)
	}
	return nil
}

// CreateIssue is not supported for JIRA in this implementation.
func (a *Adapter) CreateIssue(_ context.Context, _ domain.IssueDraft) (domain.Issue, error) {
	return domain.Issue{}, domain.ErrCreateUnsupported
}

// HasSpec returns false; JIRA has no OpenSpec concept.
func (a *Adapter) HasSpec(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// FetchAllIssues returns issues in the union of active and terminal states.
func (a *Adapter) FetchAllIssues(ctx context.Context) ([]domain.Issue, error) {
	combined := append([]string{}, a.cfg.Tracker.ActiveStates...)
	combined = append(combined, a.cfg.Tracker.TerminalStates...)
	if len(combined) == 0 {
		return nil, nil
	}
	return a.FetchIssuesByStates(ctx, combined)
}

// ---- internal helpers ----

// buildStateJQL constructs a JQL string filtering by project and status list.
// It does NOT apply JQLOverride — callers that want the override must check
// cfg.Tracker.JIRA.JQLOverride themselves.
func (a *Adapter) buildStateJQL(states []string) string {
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = `"` + s + `"`
	}
	return `project = "` + a.cfg.Tracker.JIRA.ProjectKey + `" AND status in (` + strings.Join(quoted, ",") + `)`
}

// search executes a POST /rest/api/3/search with the given JQL.
func (a *Adapter) search(ctx context.Context, jql string) ([]domain.Issue, error) {
	reqBody := map[string]any{
		"jql":        jql,
		"fields":     []string{"summary", "description", "status", "labels", "priority", "created", "updated"},
		"maxResults": maxResults,
	}

	var searchResp struct {
		Issues []jiraIssueNode `json:"issues"`
	}
	if err := a.client.Do(ctx, http.MethodPost, "/rest/api/3/search", reqBody, &searchResp); err != nil {
		return nil, err
	}

	out := make([]domain.Issue, 0, len(searchResp.Issues))
	for _, n := range searchResp.Issues {
		out = append(out, a.normalize(n))
	}
	return out, nil
}

// normalize converts a raw JIRA issue node to domain.Issue.
func (a *Adapter) normalize(n jiraIssueNode) domain.Issue {
	issue := domain.Issue{
		ID:         n.ID,
		Identifier: n.Key,
		Title:      n.Fields.Summary,
		State:      n.Fields.Status.Name,
		URL:        a.cfg.Tracker.JIRA.Endpoint + "/browse/" + n.Key,
		// TODO: map priority name to int; currently left as nil.
	}

	// Description from ADF.
	if n.Fields.Description != nil {
		issue.Description = strings.TrimRight(ADFToText(n.Fields.Description), "\n")
	}

	// Labels — lowercased.
	if len(n.Fields.Labels) > 0 {
		labels := make([]string, len(n.Fields.Labels))
		for i, l := range n.Fields.Labels {
			labels[i] = strings.ToLower(l)
		}
		issue.Labels = labels
	}

	// Timestamps.
	issue.CreatedAt = parseJIRATime(n.Fields.Created)
	issue.UpdatedAt = parseJIRATime(n.Fields.Updated)

	return issue
}

// jiraIssueNode is the raw JIRA REST API issue shape from a search response.
type jiraIssueNode struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	Fields struct {
		Summary     string   `json:"summary"`
		Description any      `json:"description"` // ADF object or null
		Labels      []string `json:"labels"`
		Created     string   `json:"created"`
		Updated     string   `json:"updated"`

		Status struct {
			Name string `json:"name"`
		} `json:"status"`

		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
	} `json:"fields"`
}

// jiraTimestampLayouts are tried in order for parsing JIRA timestamps.
// JIRA uses milliseconds and a numeric timezone offset without colon.
var jiraTimestampLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05.000Z",
	time.RFC3339Nano,
	time.RFC3339,
}

// parseJIRATime parses a JIRA timestamp string, trying multiple layouts.
// Returns nil on empty input or all-formats failure.
func parseJIRATime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range jiraTimestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
