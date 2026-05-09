// Package linear_test provides black-box tests for the Linear tracker adapter
// using an httptest.Server to avoid real network calls.
package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/tracker/linear"
	"github.com/openai/symphony/go/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ------------------------- helpers -------------------------

// capturedReq stores the request body and Authorization header of the most
// recent call made to the test server.
type capturedReq struct {
	Auth string
	Body map[string]any
}

// makeServer creates an httptest.Server that writes the provided JSON response
// on each request and captures the last request into *capturedReq.
func makeServer(t *testing.T, cap *capturedReq, responseJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.Auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&cap.Body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseJSON))
	}))
}

// baseConfig returns a minimal config wired to the test server URL.
func baseConfig(endpoint string) config.Config {
	return config.Config{
		Tracker: config.Tracker{
			Kind:         "linear",
			Endpoint:     endpoint,
			APIKey:       "test-api-key",
			ProjectSlug:  "my-project",
			ActiveStates: []string{"Todo", "In Progress"},
		},
	}
}

// newAdapter is a convenience constructor that fails the test on error.
func newAdapter(t *testing.T, cfg config.Config, httpCl *http.Client) *linear.Adapter {
	t.Helper()
	a, err := linear.New(cfg, httpCl)
	require.NoError(t, err)
	return a
}

// ========================= Test cases =========================

// --------------- 1. FetchCandidateIssues happy path ---------------

const twoIssuesResponse = `{
  "data": {
    "issues": {
      "nodes": [
        {
          "id": "issue-1",
          "identifier": "PROJ-1",
          "title": "First issue",
          "description": "desc one",
          "priority": 1,
          "state": {"name": "Todo"},
          "branchName": "feature/proj-1",
          "url": "https://linear.app/proj/PROJ-1",
          "labels": {"nodes": [{"name": "Bug"}, {"name": "URGENT"}]},
          "inverseRelations": {"nodes": []},
          "createdAt": "2024-01-01T10:00:00Z",
          "updatedAt": "2024-01-02T12:00:00Z"
        },
        {
          "id": "issue-2",
          "identifier": "PROJ-2",
          "title": "Second issue",
          "description": null,
          "priority": null,
          "state": {"name": "In Progress"},
          "branchName": null,
          "url": "https://linear.app/proj/PROJ-2",
          "labels": {"nodes": []},
          "inverseRelations": {
            "nodes": [
              {
                "type": "blocks",
                "issue": {
                  "id": "blocker-99",
                  "identifier": "PROJ-99",
                  "state": {"name": "In Progress"}
                }
              }
            ]
          },
          "createdAt": "2024-01-03T08:00:00Z",
          "updatedAt": "2024-01-03T09:00:00Z"
        }
      ]
    }
  }
}`

func TestFetchCandidateIssues_HappyPath(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, twoIssuesResponse)
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	a := newAdapter(t, cfg, srv.Client())

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	// ---- request assertions ----
	assert.Equal(t, "test-api-key", cap.Auth, "Authorization header must be raw API key (no Bearer prefix)")

	vars, ok := cap.Body["variables"].(map[string]any)
	require.True(t, ok, "request must have variables")

	// project filter
	assert.Equal(t, "my-project", vars["projectSlug"])

	// state filter list
	stateNames, ok := vars["stateNames"].([]any)
	require.True(t, ok, "stateNames must be an array")
	assert.ElementsMatch(t, []any{"Todo", "In Progress"}, stateNames)

	// ---- response assertions ----
	require.Len(t, issues, 2)

	// issue-1
	i1 := findByID(t, issues, "issue-1")
	assert.Equal(t, "PROJ-1", i1.Identifier)
	assert.Equal(t, "First issue", i1.Title)
	assert.Equal(t, "desc one", i1.Description)
	assert.Equal(t, "Todo", i1.State)
	require.NotNil(t, i1.Priority)
	assert.Equal(t, 1, *i1.Priority)
	assert.Equal(t, "feature/proj-1", i1.BranchName)
	assert.Equal(t, "https://linear.app/proj/PROJ-1", i1.URL)
	// labels must be lowercased
	assert.ElementsMatch(t, []string{"bug", "urgent"}, i1.Labels)
	// timestamps
	require.NotNil(t, i1.CreatedAt)
	assert.Equal(t, mustParseTime(t, "2024-01-01T10:00:00Z"), *i1.CreatedAt)
	require.NotNil(t, i1.UpdatedAt)
	assert.Equal(t, mustParseTime(t, "2024-01-02T12:00:00Z"), *i1.UpdatedAt)
	// no blockers
	assert.Empty(t, i1.BlockedBy)

	// issue-2
	i2 := findByID(t, issues, "issue-2")
	assert.Equal(t, "In Progress", i2.State)
	assert.Nil(t, i2.Priority)
	assert.Empty(t, i2.BranchName)
	assert.Empty(t, i2.Description)
	// blockers derived from inverseRelations where type=="blocks"
	require.Len(t, i2.BlockedBy, 1)
	assert.Equal(t, domain.BlockerRef{
		ID:         "blocker-99",
		Identifier: "PROJ-99",
		State:      "In Progress",
	}, i2.BlockedBy[0])
}

// --------------- 2. FetchCandidateIssues GraphQL error propagation ---------------

const graphqlErrorResponse = `{"errors":[{"message":"forbidden"}]}`

func TestFetchCandidateIssues_GraphQLError(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, graphqlErrorResponse)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	_, err := a.FetchCandidateIssues(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

// --------------- 3. FetchIssueStatesByIDs request shape ---------------

const oneIssueResponse = `{
  "data": {
    "issues": {
      "nodes": [
        {
          "id": "id-a",
          "identifier": "PROJ-A",
          "title": "A",
          "priority": null,
          "state": {"name": "Done"},
          "url": "https://linear.app/x/PROJ-A",
          "labels": {"nodes": []},
          "inverseRelations": {"nodes": []},
          "createdAt": "",
          "updatedAt": ""
        }
      ]
    }
  }
}`

func TestFetchIssueStatesByIDs_RequestShape(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, oneIssueResponse)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())

	ids := []string{"id-a", "id-b"}
	issues, err := a.FetchIssueStatesByIDs(context.Background(), ids)
	require.NoError(t, err)
	assert.Len(t, issues, 1)

	// Verify the query uses id.in filter
	query, _ := cap.Body["query"].(string)
	assert.Contains(t, query, "id: {in: $ids}", "query must filter by id.in")

	vars, ok := cap.Body["variables"].(map[string]any)
	require.True(t, ok)
	gotIDs, ok := vars["ids"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"id-a", "id-b"}, gotIDs)
}

func TestFetchIssueStatesByIDs_TooManyIDs(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, `{"data":{"issues":{"nodes":[]}}}`)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())

	ids := make([]string, 51)
	for i := range ids {
		ids[i] = "x"
	}
	_, err := a.FetchIssueStatesByIDs(context.Background(), ids)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "50")
}

// --------------- 4. CreateComment ---------------

func TestCreateComment_HappyPath(t *testing.T) {
	resp := `{"data":{"commentCreate":{"success":true,"userErrors":[]}}}`
	var cap capturedReq
	srv := makeServer(t, &cap, resp)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.CreateComment(context.Background(), "issue-x", "hello world")
	require.NoError(t, err)

	vars, _ := cap.Body["variables"].(map[string]any)
	assert.Equal(t, "issue-x", vars["issueId"])
	assert.Equal(t, "hello world", vars["body"])
}

func TestCreateComment_UserError(t *testing.T) {
	resp := `{"data":{"commentCreate":{"success":false,"userErrors":[{"message":"not allowed"}]}}}`
	var cap capturedReq
	srv := makeServer(t, &cap, resp)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.CreateComment(context.Background(), "issue-x", "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

// --------------- 5. UpdateIssueState two-step flow ---------------

// sequentialServer returns a server that serves responses in order (one per call).
type sequentialServer struct {
	responses []string
	idx       int
	captured  []capturedReq
}

func (s *sequentialServer) handler(w http.ResponseWriter, r *http.Request) {
	var cap capturedReq
	cap.Auth = r.Header.Get("Authorization")
	_ = json.NewDecoder(r.Body).Decode(&cap.Body)
	s.captured = append(s.captured, cap)

	if s.idx >= len(s.responses) {
		http.Error(w, "no more responses", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(s.responses[s.idx]))
	s.idx++
}

func TestUpdateIssueState_TwoStep(t *testing.T) {
	// Step 1: resolveStateID query returns state node id
	resolveResp := `{"data":{"issue":{"team":{"states":{"nodes":[{"id":"state-uuid-done"}]}}}}}`
	// Step 2: issueUpdate mutation returns success
	updateResp := `{"data":{"issueUpdate":{"success":true,"userErrors":[]}}}`

	seq := &sequentialServer{responses: []string{resolveResp, updateResp}}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.UpdateIssueState(context.Background(), "issue-z", "Done")
	require.NoError(t, err)

	require.Len(t, seq.captured, 2, "should make exactly two GraphQL calls")

	// First call: resolveStateID
	q1, _ := seq.captured[0].Body["query"].(string)
	assert.Contains(t, q1, "SymphonyResolveStateId", "first call must be the state-lookup query")
	vars1, _ := seq.captured[0].Body["variables"].(map[string]any)
	assert.Equal(t, "issue-z", vars1["issueId"])
	assert.Equal(t, "Done", vars1["stateName"])

	// Second call: issueUpdate
	q2, _ := seq.captured[1].Body["query"].(string)
	assert.Contains(t, q2, "SymphonyUpdateIssueState", "second call must be the issueUpdate mutation")
	vars2, _ := seq.captured[1].Body["variables"].(map[string]any)
	assert.Equal(t, "issue-z", vars2["issueId"])
	assert.Equal(t, "state-uuid-done", vars2["stateId"], "stateId must be the resolved ID from step 1")
}

func TestUpdateIssueState_StateNotFound(t *testing.T) {
	// State lookup returns empty nodes → no match
	resolveResp := `{"data":{"issue":{"team":{"states":{"nodes":[]}}}}}`
	seq := &sequentialServer{responses: []string{resolveResp}}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.UpdateIssueState(context.Background(), "issue-z", "NonExistentState")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "not found", "should indicate state was not found")
}

// --------------- 6. $VAR resolution end-to-end ---------------

func TestEnvVarResolution_EndToEnd(t *testing.T) {
	// Write a temporary WORKFLOW.md with $LINEAR_API_KEY indirection.
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "WORKFLOW.md")

	wfContent := `---
tracker:
  kind: linear
  endpoint: PLACEHOLDER
  api_key: $LINEAR_API_KEY_TEST_E2E
  project_slug: test-slug
agent:
  runtime: mock
---
Prompt body.
`
	require.NoError(t, os.WriteFile(wfPath, []byte(wfContent), 0o600))

	// Set the env var.
	t.Setenv("LINEAR_API_KEY_TEST_E2E", "resolved-key-xyz")

	// Capture the auth header from a test server.
	var cap capturedReq
	issueResp := `{"data":{"issues":{"nodes":[]}}}`
	srv := makeServer(t, &cap, issueResp)
	defer srv.Close()

	// Patch endpoint in the WORKFLOW.md content (rewrite with actual server URL).
	wfContent2 := strings.ReplaceAll(wfContent, "PLACEHOLDER", srv.URL)
	require.NoError(t, os.WriteFile(wfPath, []byte(wfContent2), 0o600))

	// Load → Resolve → Preflight (skipped for minimal check) → tracker.New
	wf, err := workflow.Load(wfPath)
	require.NoError(t, err)

	cfg, err := config.Resolve(wf, dir)
	require.NoError(t, err)

	assert.Equal(t, "resolved-key-xyz", cfg.Tracker.APIKey, "$VAR must be resolved")

	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, tr)

	// Exercise FetchCandidateIssues; observe resolved key in Authorization header.
	// We need to use the test server's HTTP client, but tracker.New uses nil.
	// Re-create the adapter directly with the srv client so it routes to our server.
	a, err := linear.New(cfg, srv.Client())
	require.NoError(t, err)

	_, err = a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "resolved-key-xyz", cap.Auth, "Authorization header must carry the resolved API key")
}

// --------------- 7. tracker.New selects linear ---------------

func TestTrackerNew_SelectsLinear(t *testing.T) {
	cfg := config.Config{
		Tracker: config.Tracker{
			Kind:        "linear",
			Endpoint:    "https://api.linear.app/graphql",
			APIKey:      "some-key",
			ProjectSlug: "my-proj",
		},
	}
	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

func TestTrackerNew_LinearMissingFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "missing endpoint",
			cfg: config.Config{
				Tracker: config.Tracker{
					Kind:        "linear",
					Endpoint:    "",
					APIKey:      "key",
					ProjectSlug: "slug",
				},
			},
		},
		{
			name: "missing api key",
			cfg: config.Config{
				Tracker: config.Tracker{
					Kind:        "linear",
					Endpoint:    "https://api.linear.app/graphql",
					APIKey:      "",
					ProjectSlug: "slug",
				},
			},
		},
		{
			name: "missing project slug",
			cfg: config.Config{
				Tracker: config.Tracker{
					Kind:        "linear",
					Endpoint:    "https://api.linear.app/graphql",
					APIKey:      "key",
					ProjectSlug: "",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tracker.New(tc.cfg)
			require.Error(t, err, "must error when required field is missing")
		})
	}
}

// --------------- Additional: interface compliance ---------------

func TestAdapterInterfaceCompliance(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, `{"data":{"issues":{"nodes":[]}}}`)
	defer srv.Close()

	a, err := linear.New(baseConfig(srv.URL), srv.Client())
	require.NoError(t, err)

	// Verify *Adapter satisfies tracker.Tracker
	var _ interface {
		FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error)
		FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error)
		FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error)
		CreateComment(ctx context.Context, issueID, body string) error
		UpdateIssueState(ctx context.Context, issueID, state string) error
	} = a
}

// --------------- Additional: FetchIssuesByStates ---------------

func TestFetchIssuesByStates_EmptyReturnsNil(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, `{"data":{"issues":{"nodes":[]}}}`)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	issues, err := a.FetchIssuesByStates(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, issues)
}

func TestFetchIssuesByStates_UsesCallerStates(t *testing.T) {
	var cap capturedReq
	srv := makeServer(t, &cap, `{"data":{"issues":{"nodes":[]}}}`)
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	_, err := a.FetchIssuesByStates(context.Background(), []string{"Done", "Closed"})
	require.NoError(t, err)

	vars, _ := cap.Body["variables"].(map[string]any)
	stateNames, _ := vars["stateNames"].([]any)
	assert.ElementsMatch(t, []any{"Done", "Closed"}, stateNames)
}

// ========================= test utilities =========================

func findByID(t *testing.T, issues []domain.Issue, id string) domain.Issue {
	t.Helper()
	for _, i := range issues {
		if i.ID == id {
			return i
		}
	}
	t.Fatalf("issue with id %q not found in result set", id)
	return domain.Issue{}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}
