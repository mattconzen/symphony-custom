//go:build live_jira

// Package jira_test provides an optional live smoke test against the real JIRA
// Cloud API. Run with:
//
//	SYMPHONY_RUN_LIVE_JIRA=1 \
//	  JIRA_ENDPOINT=https://acme.atlassian.net \
//	  JIRA_EMAIL=user@example.com \
//	  JIRA_API_TOKEN=<token> \
//	  JIRA_PROJECT_KEY=MYPROJ \
//	  go test ./internal/tracker/jira/... -tags live_jira -v
//
// This test is excluded from CI. It makes no assertions on content; it only
// verifies that FetchCandidateIssues succeeds with real credentials.
package jira_test

import (
	"context"
	"os"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/tracker/jira"
	"github.com/stretchr/testify/require"
)

func TestLive_FetchCandidateIssues(t *testing.T) {
	if os.Getenv("SYMPHONY_RUN_LIVE_JIRA") != "1" {
		t.Skip("set SYMPHONY_RUN_LIVE_JIRA=1 to run live tests")
	}

	endpoint := os.Getenv("JIRA_ENDPOINT")
	if endpoint == "" {
		t.Skip("JIRA_ENDPOINT not set")
	}
	email := os.Getenv("JIRA_EMAIL")
	if email == "" {
		t.Skip("JIRA_EMAIL not set")
	}
	apiToken := os.Getenv("JIRA_API_TOKEN")
	if apiToken == "" {
		t.Skip("JIRA_API_TOKEN not set")
	}
	projectKey := os.Getenv("JIRA_PROJECT_KEY")
	if projectKey == "" {
		t.Skip("JIRA_PROJECT_KEY not set")
	}

	cfg := config.Config{
		Tracker: config.Tracker{
			Kind:         "jira",
			ActiveStates: []string{"Todo", "In Progress"},
			JIRA: config.TrackerJIRA{
				Endpoint:   endpoint,
				Email:      email,
				APIToken:   apiToken,
				ProjectKey: projectKey,
			},
		},
	}

	a, err := jira.New(cfg, nil)
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	t.Logf("fetched %d candidate issues from project %q", len(issues), projectKey)
	for _, iss := range issues {
		t.Logf("  %s (%s): %s [%s]", iss.Identifier, iss.ID, iss.Title, iss.State)
	}
}
