package domain

import "time"

// RunAttempt is one execution attempt for one issue per SPEC §4.1.5.
type RunAttempt struct {
	IssueID         string
	IssueIdentifier string
	// Attempt is nil for first run, >=1 for retries/continuation.
	Attempt       *int
	WorkspacePath string
	StartedAt     time.Time
	Status        string
	Error         string
}

// LiveSession holds state tracked while a coding-agent subprocess is running
// per SPEC §4.1.6.
type LiveSession struct {
	SessionID    string
	ThreadID     string
	TurnID       string
	AgentPID     string
	LastEvent    string
	LastEventAt  *time.Time
	LastMessage  string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	TurnCount    int
}

// RetryEntry is the scheduled retry state for an issue per SPEC §4.1.7.
type RetryEntry struct {
	IssueID    string
	Identifier string
	Attempt    int
	DueAtMs    int64
	Error      string
}
