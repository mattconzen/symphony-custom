// Package jira_test provides black-box tests for the JIRA Cloud REST v3 tracker
// adapter using an httptest.Server to avoid real network calls.
package jira_test

import (
	"context"
	"encoding/base64"
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
	"github.com/openai/symphony/go/internal/tracker/jira"
	"github.com/openai/symphony/go/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ========================= helpers =========================

// reqCapture stores a captured HTTP request from the test server.
type reqCapture struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// sequentialServer serves a scripted sequence of responses, one per request.
type sequentialServer struct {
	responses []func(w http.ResponseWriter, r *http.Request)
	captures  []reqCapture
	idx       int
}

func (s *sequentialServer) handler(w http.ResponseWriter, r *http.Request) {
	var cap reqCapture
	cap.Method = r.Method
	cap.Path = r.URL.Path
	cap.Auth = r.Header.Get("Authorization")

	// Decode body (may be absent for GET).
	_ = json.NewDecoder(r.Body).Decode(&cap.Body)
	s.captures = append(s.captures, cap)

	if s.idx >= len(s.responses) {
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	s.responses[s.idx](w, r)
	s.idx++
}

// jsonResp returns a handler function that writes JSON with status 200.
func jsonResp(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// noContentResp returns a 204 No Content response.
func noContentResp() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// expectedBasicAuth returns the expected Basic auth header value for email:token.
func expectedBasicAuth(email, token string) string {
	creds := email + ":" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// baseConfig returns a minimal JIRA config wired to the test server URL.
func baseConfig(endpoint string) config.Config {
	return config.Config{
		Tracker: config.Tracker{
			Kind:         "jira",
			ActiveStates: []string{"Todo", "In Progress"},
			JIRA: config.TrackerJIRA{
				Endpoint:   endpoint,
				Email:      "test@example.com",
				APIToken:   "test-token",
				ProjectKey: "MT",
			},
		},
	}
}

// newAdapter is a convenience constructor that fails the test on error.
func newAdapter(t *testing.T, cfg config.Config, httpCl *http.Client) *jira.Adapter {
	t.Helper()
	a, err := jira.New(cfg, httpCl)
	require.NoError(t, err)
	return a
}

// mustParseTime parses an RFC3339 string, failing the test on error.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}

// findByID searches for an issue by JIRA numeric id in the slice.
func findByID(t *testing.T, issues []domain.Issue, id string) domain.Issue {
	t.Helper()
	for _, iss := range issues {
		if iss.ID == id {
			return iss
		}
	}
	t.Fatalf("issue with id %q not found in result set", id)
	return domain.Issue{}
}

// ========================= Test responses =========================

// twoIssuesResp is a search response with two issues (one with ADF description).
const twoIssuesResp = `{
  "issues": [
    {
      "id": "10001",
      "key": "MT-1",
      "fields": {
        "summary": "First issue",
        "description": {
          "type": "doc",
          "version": 1,
          "content": [
            {
              "type": "paragraph",
              "content": [
                {"type": "text", "text": "Hello world"}
              ]
            }
          ]
        },
        "status": {"name": "Todo"},
        "labels": ["Bug", "URGENT"],
        "created": "2024-01-01T10:00:00.000+0000",
        "updated": "2024-01-02T12:00:00.000+0000"
      }
    },
    {
      "id": "10002",
      "key": "MT-2",
      "fields": {
        "summary": "Second issue",
        "description": null,
        "status": {"name": "In Progress"},
        "labels": [],
        "created": "2024-01-03T08:00:00.000+0000",
        "updated": "2024-01-03T09:00:00.000+0000"
      }
    }
  ]
}`

// ========================= Test cases =========================

// --------------- 1. FetchCandidateIssues happy path ---------------

func TestFetchCandidateIssues_HappyPath(t *testing.T) {
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(twoIssuesResp),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	a := newAdapter(t, cfg, srv.Client())

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	// ---- request assertions ----
	require.Len(t, seq.captures, 1)
	cap := seq.captures[0]

	assert.Equal(t, http.MethodPost, cap.Method, "FetchCandidateIssues must POST")
	assert.Equal(t, "/rest/api/3/search", cap.Path)
	assert.Equal(t, expectedBasicAuth("test@example.com", "test-token"), cap.Auth,
		"Authorization must be Basic base64(email:token)")

	// JQL must include project key and active states
	jqlRaw, _ := cap.Body["jql"].(string)
	assert.Contains(t, jqlRaw, "MT", "JQL must reference the project key")
	assert.Contains(t, jqlRaw, "Todo", "JQL must include active state Todo")
	assert.Contains(t, jqlRaw, "In Progress", "JQL must include active state In Progress")

	// ---- response assertions ----
	require.Len(t, issues, 2)

	i1 := findByID(t, issues, "10001")
	assert.Equal(t, "MT-1", i1.Identifier)
	assert.Equal(t, "First issue", i1.Title)
	assert.Equal(t, "Hello world", i1.Description, "ADF should convert to plain text")
	assert.Equal(t, "Todo", i1.State)
	assert.Equal(t, srv.URL+"/browse/MT-1", i1.URL)
	assert.ElementsMatch(t, []string{"bug", "urgent"}, i1.Labels, "labels must be lowercased")
	require.NotNil(t, i1.CreatedAt)
	assert.Equal(t, mustParseTime(t, "2024-01-01T10:00:00Z"), i1.CreatedAt.UTC())
	require.NotNil(t, i1.UpdatedAt)
	assert.Equal(t, mustParseTime(t, "2024-01-02T12:00:00Z"), i1.UpdatedAt.UTC())

	i2 := findByID(t, issues, "10002")
	assert.Equal(t, "MT-2", i2.Identifier)
	assert.Equal(t, "In Progress", i2.State)
	assert.Empty(t, i2.Description, "null description should produce empty string")
	assert.Empty(t, i2.Labels)
}

// --------------- 2. JQLOverride is used verbatim ---------------

func TestFetchCandidateIssues_JQLOverride(t *testing.T) {
	const overrideJQL = "project = CUSTOM AND assignee = currentUser()"

	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(`{"issues":[]}`),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	cfg.Tracker.JIRA.JQLOverride = overrideJQL
	a := newAdapter(t, cfg, srv.Client())

	_, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	require.Len(t, seq.captures, 1)
	jqlRaw, _ := seq.captures[0].Body["jql"].(string)
	assert.Equal(t, overrideJQL, jqlRaw, "JQLOverride must be used verbatim")
}

// --------------- 3. FetchIssueStatesByIDs request shape ---------------

func TestFetchIssueStatesByIDs_RequestShape(t *testing.T) {
	const oneIssueResp = `{
  "issues": [
    {
      "id": "10001",
      "key": "MT-1",
      "fields": {
        "summary": "Issue 1",
        "description": null,
        "status": {"name": "Done"},
        "labels": [],
        "created": "",
        "updated": ""
      }
    }
  ]
}`
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(oneIssueResp),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())

	issues, err := a.FetchIssueStatesByIDs(context.Background(), []string{"MT-1", "MT-2"})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Done", issues[0].State)

	// Verify JQL uses key in (...)
	jqlRaw, _ := seq.captures[0].Body["jql"].(string)
	assert.Contains(t, jqlRaw, `"MT-1"`, "JQL must quote key MT-1")
	assert.Contains(t, jqlRaw, `"MT-2"`, "JQL must quote key MT-2")
	assert.Contains(t, strings.ToLower(jqlRaw), "key in", "JQL must use key in (...)")
}

func TestFetchIssueStatesByIDs_EmptyReturnsNil(t *testing.T) {
	// No server needed — empty IDs should return nil without making a request.
	cfg := baseConfig("http://127.0.0.1:0") // unreachable; should never be called
	a := newAdapter(t, cfg, nil)

	issues, err := a.FetchIssueStatesByIDs(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, issues)
}

// --------------- 4. CreateComment happy path ---------------

func TestCreateComment_HappyPath(t *testing.T) {
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(`{"id": "1001", "body": {}}`),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.CreateComment(context.Background(), "MT-1", "Hello from Symphony")
	require.NoError(t, err)

	require.Len(t, seq.captures, 1)
	cap := seq.captures[0]

	assert.Equal(t, http.MethodPost, cap.Method)
	assert.Equal(t, "/rest/api/3/issue/MT-1/comment", cap.Path)

	// Verify body has ADF structure: body.type == "doc"
	bodyField, ok := cap.Body["body"].(map[string]any)
	require.True(t, ok, "request body must have a 'body' field")
	assert.Equal(t, "doc", bodyField["type"], "ADF root node type must be 'doc'")

	// Walk to find the text node
	content, _ := bodyField["content"].([]any)
	require.NotEmpty(t, content)
	para, _ := content[0].(map[string]any)
	assert.Equal(t, "paragraph", para["type"])
	paraContent, _ := para["content"].([]any)
	require.NotEmpty(t, paraContent)
	textNode, _ := paraContent[0].(map[string]any)
	assert.Equal(t, "text", textNode["type"])
	assert.Equal(t, "Hello from Symphony", textNode["text"])
}

// --------------- 5. UpdateIssueState two-step flow ---------------

const transitionsResp = `{
  "transitions": [
    {"id": "31", "name": "Done", "to": {"name": "Done"}},
    {"id": "21", "name": "Start Progress", "to": {"name": "In Progress"}}
  ]
}`

func TestUpdateIssueState_TwoStep(t *testing.T) {
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(transitionsResp), // Step 1: GET /transitions
			noContentResp(),           // Step 2: POST /transitions → 204
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.UpdateIssueState(context.Background(), "MT-1", "Done")
	require.NoError(t, err)

	require.Len(t, seq.captures, 2, "must make exactly two requests")

	// First request: GET /transitions
	cap1 := seq.captures[0]
	assert.Equal(t, http.MethodGet, cap1.Method)
	assert.Equal(t, "/rest/api/3/issue/MT-1/transitions", cap1.Path)

	// Second request: POST /transitions with transition id "31"
	cap2 := seq.captures[1]
	assert.Equal(t, http.MethodPost, cap2.Method)
	assert.Equal(t, "/rest/api/3/issue/MT-1/transitions", cap2.Path)

	transition, ok := cap2.Body["transition"].(map[string]any)
	require.True(t, ok, "request body must have 'transition' field")
	assert.Equal(t, "31", transition["id"], "must use the transition id matching 'Done'")
}

func TestUpdateIssueState_StateNotFound(t *testing.T) {
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(`{"transitions": [{"id": "21", "name": "Start Progress", "to": {"name": "In Progress"}}]}`),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.UpdateIssueState(context.Background(), "MT-1", "Done")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Done", "error must mention the missing state name")
	assert.Contains(t, strings.ToLower(err.Error()), "not found", "error must say state not found")
}

func TestUpdateIssueState_CaseInsensitive(t *testing.T) {
	// Target state "in progress" (lowercase) should match "In Progress" in transitions.
	seq := &sequentialServer{
		responses: []func(http.ResponseWriter, *http.Request){
			jsonResp(transitionsResp),
			noContentResp(),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(seq.handler))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	err := a.UpdateIssueState(context.Background(), "MT-1", "in progress")
	require.NoError(t, err)

	require.Len(t, seq.captures, 2)
	transition, _ := seq.captures[1].Body["transition"].(map[string]any)
	assert.Equal(t, "21", transition["id"])
}

// --------------- 6. $VAR resolution end-to-end ---------------

func TestEnvVarResolution_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "WORKFLOW.md")

	wfContent := `---
tracker:
  kind: jira
  jira:
    endpoint: PLACEHOLDER
    email: user@example.com
    api_token: $JIRA_API_TOKEN_TEST_E2E
    project_key: TEST
agent:
  runtime: mock
---
Prompt body.
`
	require.NoError(t, os.WriteFile(wfPath, []byte(wfContent), 0o600))
	t.Setenv("JIRA_API_TOKEN_TEST_E2E", "resolved-token-xyz")

	// Start test server.
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer srv.Close()

	// Patch endpoint in workflow.
	wfContent2 := strings.ReplaceAll(wfContent, "PLACEHOLDER", srv.URL)
	require.NoError(t, os.WriteFile(wfPath, []byte(wfContent2), 0o600))

	wf, err := workflow.Load(wfPath)
	require.NoError(t, err)

	cfg, err := config.Resolve(wf, dir)
	require.NoError(t, err)

	assert.Equal(t, "resolved-token-xyz", cfg.Tracker.JIRA.APIToken, "$VAR must be resolved")
	assert.Equal(t, "user@example.com", cfg.Tracker.JIRA.Email)

	// Build adapter and exercise FetchCandidateIssues.
	a, err := jira.New(cfg, srv.Client())
	require.NoError(t, err)

	_, err = a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)

	expected := expectedBasicAuth("user@example.com", "resolved-token-xyz")
	assert.Equal(t, expected, capturedAuth,
		"Authorization header must carry the resolved token via Basic auth")
}

// --------------- 7. tracker.New selects jira ---------------

func TestTrackerNew_SelectsJIRA(t *testing.T) {
	cfg := config.Config{
		Tracker: config.Tracker{
			Kind: "jira",
			JIRA: config.TrackerJIRA{
				Endpoint:   "https://acme.atlassian.net",
				Email:      "admin@example.com",
				APIToken:   "tok",
				ProjectKey: "PROJ",
			},
		},
	}
	tr, err := tracker.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, tr)
}

// --------------- 8. Preflight rejects missing required fields ---------------

func TestPreflight_JIRA_MissingFields(t *testing.T) {
	cases := []struct {
		name   string
		modify func(j *config.TrackerJIRA)
		errMsg string
	}{
		{
			name:   "missing endpoint",
			modify: func(j *config.TrackerJIRA) { j.Endpoint = "" },
			errMsg: "endpoint",
		},
		{
			name:   "missing email",
			modify: func(j *config.TrackerJIRA) { j.Email = "" },
			errMsg: "email",
		},
		{
			name:   "missing api_token",
			modify: func(j *config.TrackerJIRA) { j.APIToken = "" },
			errMsg: "api_token",
		},
		{
			name:   "missing project_key",
			modify: func(j *config.TrackerJIRA) { j.ProjectKey = "" },
			errMsg: "project_key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Tracker: config.Tracker{
					Kind: "jira",
					JIRA: config.TrackerJIRA{
						Endpoint:   "https://acme.atlassian.net",
						Email:      "admin@example.com",
						APIToken:   "tok",
						ProjectKey: "PROJ",
					},
				},
				Agent: config.Agent{Runtime: "mock"},
			}
			tc.modify(&cfg.Tracker.JIRA)
			err := config.Preflight(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}

// --------------- Additional: non-2xx error propagation ---------------

func TestDo_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errorMessages":["Issue does not exist"]}`, http.StatusNotFound)
	}))
	defer srv.Close()

	a := newAdapter(t, baseConfig(srv.URL), srv.Client())
	_, err := a.FetchCandidateIssues(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

// --------------- Additional: ADF helpers ---------------

func TestADFToText_Paragraph(t *testing.T) {
	// paragraph with two text nodes separated by a hardBreak
	node := map[string]any{
		"type": "paragraph",
		"content": []any{
			map[string]any{"type": "text", "text": "Line one"},
			map[string]any{"type": "hardBreak"},
			map[string]any{"type": "text", "text": "Line two"},
		},
	}
	result := jira.ADFToText(node)
	assert.Equal(t, "Line one\nLine two\n", result)
}

func TestADFToText_NilReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", jira.ADFToText(nil))
}

func TestADFToText_DocWithMultipleParagraphs(t *testing.T) {
	node := map[string]any{
		"type": "doc",
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "First"},
				},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Second"},
				},
			},
		},
	}
	result := jira.ADFToText(node)
	// doc has no trailing newline; each paragraph adds one
	assert.Equal(t, "First\nSecond\n", result)
}

func TestTextToADF_Structure(t *testing.T) {
	adf := jira.TextToADF("hello")
	m, ok := adf.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "doc", m["type"])
	assert.Equal(t, 1, m["version"])

	content, ok := m["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	para, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "paragraph", para["type"])

	paraContent, ok := para["content"].([]any)
	require.True(t, ok)
	require.Len(t, paraContent, 1)

	textNode, ok := paraContent[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", textNode["type"])
	assert.Equal(t, "hello", textNode["text"])
}
