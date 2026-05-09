//go:build live_linear

// Package linear_test provides an optional live smoke test against the real
// Linear API. Run with:
//
//	SYMPHONY_RUN_LIVE_LINEAR=1 LINEAR_API_KEY=<key> SYMPHONY_LINEAR_PROJECT_SLUG=<slug> \
//	  go test ./internal/tracker/linear/... -tags live_linear -v
//
// This test is excluded from CI. It makes no assertions on content; it only
// verifies that FetchCandidateIssues succeeds with real credentials.
package linear_test

import (
	"context"
	"os"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/tracker/linear"
	"github.com/stretchr/testify/require"
)

func TestLive_FetchCandidateIssues(t *testing.T) {
	if os.Getenv("SYMPHONY_RUN_LIVE_LINEAR") != "1" {
		t.Skip("set SYMPHONY_RUN_LIVE_LINEAR=1 to run live tests")
	}

	apiKey := os.Getenv("LINEAR_API_KEY")
	if apiKey == "" {
		t.Skip("LINEAR_API_KEY not set")
	}

	projectSlug := os.Getenv("SYMPHONY_LINEAR_PROJECT_SLUG")
	if projectSlug == "" {
		t.Skip("SYMPHONY_LINEAR_PROJECT_SLUG not set")
	}

	cfg := config.Config{
		Tracker: config.Tracker{
			Kind:         "linear",
			Endpoint:     "https://api.linear.app/graphql",
			APIKey:       apiKey,
			ProjectSlug:  projectSlug,
			ActiveStates: []string{"Todo", "In Progress"},
		},
	}

	a, err := linear.New(cfg, nil)
	require.NoError(t, err)

	issues, err := a.FetchCandidateIssues(context.Background())
	require.NoError(t, err)
	t.Logf("fetched %d candidate issues from project %q", len(issues), projectSlug)
}
