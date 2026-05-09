package linear

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// Maximum issues returned per page. FetchIssueStatesByIDs asserts len(ids) <= pageSize.
const pageSize = 50

// GraphQL query/mutation constants. Kept inline per SPEC §11.2 so that query
// construction is isolated and easy to audit.

// queryFetchIssues fetches candidate issues filtered by project slug and state names.
// Mirrors the Elixir SymphonyLinearPoll query but without pagination (single page, first 50).
const queryFetchIssues = `
query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!) {
  issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first) {
    nodes {
      id
      identifier
      title
      description
      priority
      state {
        name
      }
      branchName
      url
      labels {
        nodes {
          name
        }
      }
      inverseRelations(first: 50) {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
      createdAt
      updatedAt
    }
  }
}`

// queryFetchIssuesByIDs fetches issues by their Linear node IDs for reconciliation.
// Asserts len(ids) <= 50 (pageSize). Mirrors SymphonyLinearIssuesById.
const queryFetchIssuesByIDs = `
query SymphonyLinearIssuesById($ids: [ID!]!, $first: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      identifier
      title
      description
      priority
      state {
        name
      }
      branchName
      url
      labels {
        nodes {
          name
        }
      }
      inverseRelations(first: 50) {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
      createdAt
      updatedAt
    }
  }
}`

// mutationCreateComment appends a comment to a Linear issue.
const mutationCreateComment = `
mutation SymphonyCreateComment($issueId: String!, $body: String!) {
  commentCreate(input: {issueId: $issueId, body: $body}) {
    success
    userErrors {
      message
    }
  }
}`

// queryResolveStateID resolves a workflow-state ID given an issue ID and a state
// name. Uses issue → team → states to stay team-scoped per the Elixir adapter.
const queryResolveStateID = `
query SymphonyResolveStateId($issueId: String!, $stateName: String!) {
  issue(id: $issueId) {
    team {
      states(filter: {name: {eq: $stateName}}, first: 1) {
        nodes {
          id
        }
      }
    }
  }
}`

// mutationUpdateIssueState transitions an issue to the state identified by stateId.
const mutationUpdateIssueState = `
mutation SymphonyUpdateIssueState($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: {stateId: $stateId}) {
    success
    userErrors {
      message
    }
  }
}`

// Adapter implements tracker.Tracker for Linear.
type Adapter struct {
	cfg    config.Config
	client *Client
}

// New creates a new Linear Adapter. httpClient may be nil; a default 30 s client
// will be used. Returns an error if required fields (Endpoint, APIKey,
// ProjectSlug) are missing.
func New(cfg config.Config, httpClient *http.Client) (*Adapter, error) {
	if cfg.Tracker.Endpoint == "" {
		return nil, fmt.Errorf("linear: tracker.endpoint is required")
	}
	if cfg.Tracker.APIKey == "" {
		return nil, fmt.Errorf("linear: tracker.api_key is required")
	}
	if cfg.Tracker.ProjectSlug == "" {
		return nil, fmt.Errorf("linear: tracker.project_slug is required")
	}

	c := &Client{
		Endpoint: cfg.Tracker.Endpoint,
		APIKey:   cfg.Tracker.APIKey,
		HTTP:     httpClient,
	}
	return &Adapter{cfg: cfg, client: c}, nil
}

// FetchCandidateIssues returns issues in the configured active states for the
// configured project. Single page, first 50 issues. Pagination is not
// implemented in Phase 2; a TODO is left for follow-up.
//
// TODO(phase3): add cursor-based pagination to match full Elixir behaviour.
func (a *Adapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	vars := map[string]any{
		"projectSlug": a.cfg.Tracker.ProjectSlug,
		"stateNames":  a.cfg.Tracker.ActiveStates,
		"first":       pageSize,
	}

	var data struct {
		Issues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := a.client.Do(ctx, queryFetchIssues, vars, &data); err != nil {
		return nil, err
	}
	return normalizeNodes(data.Issues.Nodes), nil
}

// FetchIssuesByStates returns issues in the given states for the configured project.
// Delegates to a filtered query with caller-supplied states.
// Single page, first 50 issues.
func (a *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}

	vars := map[string]any{
		"projectSlug": a.cfg.Tracker.ProjectSlug,
		"stateNames":  states,
		"first":       pageSize,
	}

	var data struct {
		Issues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := a.client.Do(ctx, queryFetchIssues, vars, &data); err != nil {
		return nil, err
	}
	return normalizeNodes(data.Issues.Nodes), nil
}

// FetchIssueStatesByIDs returns the current state for each given issue ID.
// Asserts len(ids) <= 50; callers with larger sets must batch externally.
// Used for reconciliation of active runs.
func (a *Adapter) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > pageSize {
		return nil, fmt.Errorf("linear: FetchIssueStatesByIDs supports at most %d IDs per call (got %d)", pageSize, len(ids))
	}

	vars := map[string]any{
		"ids":   ids,
		"first": len(ids),
	}

	var data struct {
		Issues struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := a.client.Do(ctx, queryFetchIssuesByIDs, vars, &data); err != nil {
		return nil, err
	}
	return normalizeNodes(data.Issues.Nodes), nil
}

// CreateComment appends a comment to the given issue. Returns an error if the
// mutation reports a userError or success == false.
func (a *Adapter) CreateComment(ctx context.Context, issueID, body string) error {
	vars := map[string]any{
		"issueId": issueID,
		"body":    body,
	}

	var data struct {
		CommentCreate struct {
			Success    bool        `json:"success"`
			UserErrors []userError `json:"userErrors"`
		} `json:"commentCreate"`
	}
	if err := a.client.Do(ctx, mutationCreateComment, vars, &data); err != nil {
		return err
	}
	if len(data.CommentCreate.UserErrors) > 0 {
		return fmt.Errorf("linear: createComment userError: %s", data.CommentCreate.UserErrors[0].Message)
	}
	if !data.CommentCreate.Success {
		return fmt.Errorf("linear: createComment returned success=false")
	}
	return nil
}

// UpdateIssueState transitions an issue to the named state. It resolves the
// workflow-state ID via a two-step approach:
//  1. queryResolveStateID: look up the state node ID via issue → team → states.
//  2. mutationUpdateIssueState: apply the resolved stateId.
//
// This mirrors the Elixir adapter's resolve_state_id/update_issue_state flow exactly.
func (a *Adapter) UpdateIssueState(ctx context.Context, issueID, stateName string) error {
	stateID, err := a.resolveStateID(ctx, issueID, stateName)
	if err != nil {
		return err
	}

	vars := map[string]any{
		"issueId": issueID,
		"stateId": stateID,
	}

	var data struct {
		IssueUpdate struct {
			Success    bool        `json:"success"`
			UserErrors []userError `json:"userErrors"`
		} `json:"issueUpdate"`
	}
	if err := a.client.Do(ctx, mutationUpdateIssueState, vars, &data); err != nil {
		return err
	}
	if len(data.IssueUpdate.UserErrors) > 0 {
		return fmt.Errorf("linear: issueUpdate userError: %s", data.IssueUpdate.UserErrors[0].Message)
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate returned success=false")
	}
	return nil
}

// resolveStateID uses queryResolveStateID to look up the state node ID for
// stateName within the team that owns issueID.
func (a *Adapter) resolveStateID(ctx context.Context, issueID, stateName string) (string, error) {
	vars := map[string]any{
		"issueId":   issueID,
		"stateName": stateName,
	}

	var data struct {
		Issue struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := a.client.Do(ctx, queryResolveStateID, vars, &data); err != nil {
		return "", err
	}
	nodes := data.Issue.Team.States.Nodes
	if len(nodes) == 0 {
		return "", fmt.Errorf("linear: state %q not found for issue %s", stateName, issueID)
	}
	return nodes[0].ID, nil
}

// ---- normalization helpers ----

// userError mirrors the Linear userErrors field returned by mutations.
type userError struct {
	Message string `json:"message"`
}

// linearIssueNode is the raw GraphQL issue node shape from Linear.
type linearIssueNode struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Priority    *int    `json:"priority"`
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	BranchName *string `json:"branchName"`
	URL        string  `json:"url"`
	Labels     struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	InverseRelations struct {
		Nodes []struct {
			Type  string `json:"type"`
			Issue struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				State      struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// normalizeNodes converts a slice of raw Linear issue nodes to domain.Issue.
func normalizeNodes(nodes []linearIssueNode) []domain.Issue {
	out := make([]domain.Issue, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, normalize(n))
	}
	return out
}

// normalize converts a single raw Linear issue node to domain.Issue per SPEC §11.3.
func normalize(n linearIssueNode) domain.Issue {
	issue := domain.Issue{
		ID:         n.ID,
		Identifier: n.Identifier,
		Title:      n.Title,
		State:      n.State.Name,
		URL:        n.URL,
		Priority:   n.Priority,
	}

	// Nullable string fields: use zero value when nil
	if n.Description != nil {
		issue.Description = *n.Description
	}
	if n.BranchName != nil {
		issue.BranchName = *n.BranchName
	}

	// Labels — lowercase per SPEC §11.3
	if len(n.Labels.Nodes) > 0 {
		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, lbl := range n.Labels.Nodes {
			labels = append(labels, strings.ToLower(lbl.Name))
		}
		issue.Labels = labels
	}

	// blocked_by — derived from inverseRelations where type == "blocks" per SPEC §11.3
	for _, rel := range n.InverseRelations.Nodes {
		if strings.ToLower(strings.TrimSpace(rel.Type)) == "blocks" {
			issue.BlockedBy = append(issue.BlockedBy, domain.BlockerRef{
				ID:         rel.Issue.ID,
				Identifier: rel.Issue.Identifier,
				State:      rel.Issue.State.Name,
			})
		}
	}

	// Timestamps — parse RFC 3339 / ISO-8601
	issue.CreatedAt = parseTime(n.CreatedAt)
	issue.UpdatedAt = parseTime(n.UpdatedAt)

	return issue
}

// parseTime parses an ISO-8601 / RFC 3339 timestamp string.
// Returns nil on empty input or parse failure.
func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
