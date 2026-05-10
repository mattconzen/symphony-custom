package observability

import "time"

// Snapshot is the read-only projection of orchestrator state consumed by the
// observability web dashboard and JSON API. Field names mirror the Elixir
// SymphonyElixirWeb.Presenter.state_payload/2 output exactly so the API
// contract is stable across runtimes.
type Snapshot struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Counts      Counts         `json:"counts"`
	CodexTotals TokenTotals    `json:"codex_totals"`
	RateLimits  any            `json:"rate_limits"`
	Running     []RunningEntry `json:"running"`
	Retrying    []RetryEntry   `json:"retrying"`
	Polling     Polling        `json:"polling"`
}

// Polling mirrors Elixir Presenter's `polling` map: a snapshot of the
// orchestrator poll-loop state surfaced in the dashboard header.
type Polling struct {
	Checking       bool `json:"checking"`
	PollIntervalMs int  `json:"poll_interval_ms"`
	NextPollInMs   int  `json:"next_poll_in_ms"`
}

// Counts mirrors Elixir snapshot.counts.
type Counts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

// TokenTotals mirrors Elixir snapshot.codex_totals (global aggregation).
// Carries seconds_running which the dashboard surfaces as cumulative runtime.
type TokenTotals struct {
	TotalTokens    int `json:"total_tokens"`
	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
	SecondsRunning int `json:"seconds_running"`
}

// EntryTokens mirrors the per-running-entry tokens map in Elixir's
// running_entry_payload (no seconds_running key).
type EntryTokens struct {
	TotalTokens  int `json:"total_tokens"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// RunningEntry mirrors Elixir snapshot.running[]. Field names match the
// Presenter running_entry_payload shape exactly. Pointer fields encode JSON
// `null` when unset, matching the Elixir output for absent keys.
type RunningEntry struct {
	IssueID         string      `json:"issue_id"`
	IssueIdentifier string      `json:"issue_identifier"`
	State           string      `json:"state"`
	WorkerHost      *string     `json:"worker_host"`
	WorkspacePath   *string     `json:"workspace_path"`
	SessionID       *string     `json:"session_id"`
	TurnCount       int         `json:"turn_count"`
	LastEvent       *string     `json:"last_event"`
	LastMessage     *string     `json:"last_message"`
	StartedAt       *time.Time  `json:"started_at"`
	LastEventAt     *time.Time  `json:"last_event_at"`
	Tokens          EntryTokens `json:"tokens"`
}

// RetryEntry mirrors Elixir snapshot.retrying[]. Field names match the
// Presenter retry_entry_payload shape exactly.
type RetryEntry struct {
	IssueID         string     `json:"issue_id"`
	IssueIdentifier string     `json:"issue_identifier"`
	Attempt         int        `json:"attempt"`
	DueAt           *time.Time `json:"due_at"`
	Error           *string    `json:"error"`
	WorkerHost      *string    `json:"worker_host"`
	WorkspacePath   *string    `json:"workspace_path"`
}
