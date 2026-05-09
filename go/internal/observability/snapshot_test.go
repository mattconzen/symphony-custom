package observability_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/observability"
)

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// timePtr returns a pointer to t.
func timePtr(t time.Time) *time.Time { return &t }

// mustParseRFC3339 parses an RFC3339 timestamp or fails the test.
func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// fixedSnapshot builds the Snapshot value that the testdata golden file
// represents. Keeping this in one helper lets BuildSnapshot tests reuse it.
func fixedSnapshot(t *testing.T) observability.Snapshot {
	t.Helper()
	return observability.Snapshot{
		GeneratedAt: mustParseRFC3339(t, "2026-05-09T12:00:00Z"),
		Counts:      observability.Counts{Running: 2, Retrying: 1},
		CodexTotals: observability.TokenTotals{
			TotalTokens:    1500,
			InputTokens:    900,
			OutputTokens:   600,
			SecondsRunning: 42,
		},
		RateLimits: map[string]any{
			"primary": map[string]any{
				"used_percent": 12.5,
				"resets_at":    "2026-05-09T13:00:00Z",
			},
			"secondary": nil,
		},
		Running: []observability.RunningEntry{
			{
				IssueID:         "issue-1",
				IssueIdentifier: "TEST-1",
				State:           "In Progress",
				WorkerHost:      strPtr("worker-a"),
				WorkspacePath:   strPtr("/tmp/work/TEST-1"),
				SessionID:       strPtr("sess-1"),
				TurnCount:       2,
				LastEvent:       strPtr("assistant_message"),
				LastMessage:     strPtr("Refactoring the parser."),
				StartedAt:       timePtr(mustParseRFC3339(t, "2026-05-09T11:55:00Z")),
				LastEventAt:     timePtr(mustParseRFC3339(t, "2026-05-09T11:59:30Z")),
				Tokens: observability.EntryTokens{
					TotalTokens:  800,
					InputTokens:  500,
					OutputTokens: 300,
				},
			},
			{
				IssueID:         "issue-2",
				IssueIdentifier: "TEST-2",
				State:           "In Progress",
				WorkerHost:      nil,
				WorkspacePath:   strPtr("/tmp/work/TEST-2"),
				SessionID:       strPtr("sess-2"),
				TurnCount:       1,
				LastEvent:       strPtr("tool_call"),
				LastMessage:     nil,
				StartedAt:       timePtr(mustParseRFC3339(t, "2026-05-09T11:58:00Z")),
				LastEventAt:     timePtr(mustParseRFC3339(t, "2026-05-09T11:59:45Z")),
				Tokens: observability.EntryTokens{
					TotalTokens:  700,
					InputTokens:  400,
					OutputTokens: 300,
				},
			},
		},
		Retrying: []observability.RetryEntry{
			{
				IssueID:         "issue-3",
				IssueIdentifier: "TEST-3",
				Attempt:         2,
				DueAt:           timePtr(mustParseRFC3339(t, "2026-05-09T12:05:00Z")),
				Error:           strPtr("transient agent failure"),
				WorkerHost:      nil,
				WorkspacePath:   nil,
			},
		},
	}
}

func TestSnapshot_GoldenJSON(t *testing.T) {
	snap := fixedSnapshot(t)

	got, err := json.MarshalIndent(snap, "", "  ")
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "snapshot.golden.json")
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err)

	// Trim trailing newlines so the comparison is robust against editor settings.
	gotTrimmed := bytes.TrimRight(got, "\n")
	wantTrimmed := bytes.TrimRight(want, "\n")

	if !bytes.Equal(gotTrimmed, wantTrimmed) {
		t.Fatalf("snapshot JSON mismatch.\n--- got ---\n%s\n--- want ---\n%s", gotTrimmed, wantTrimmed)
	}
}
