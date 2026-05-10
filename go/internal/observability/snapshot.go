package observability

import (
	"time"

	"github.com/openai/symphony/go/internal/domain"
)

// Snapshot is the read-only projection of orchestrator state consumed by the
// observability web dashboard and JSON API.
//
// **Divergence from Elixir:** the Go dashboard renames `codex_totals` to
// `agent_totals` (and `codex_session_logs` to `agent_session_logs`) so the
// surface is provider-agnostic. The Elixir reference implementation under
// `elixir/` retains the `codex_*` keys; consumers that need to support both
// runtimes must translate.
type Snapshot struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Counts      Counts         `json:"counts"`
	AgentTotals TokenTotals    `json:"agent_totals"`
	RateLimits  any            `json:"rate_limits"`
	Running     []RunningEntry `json:"running"`
	Retrying    []RetryEntry   `json:"retrying"`
	Polling     Polling        `json:"polling"`
	Kanban      []KanbanColumn `json:"kanban"`
}

// KanbanColumn is one column of the Kanban view. Cards are sorted by
// identifier within a column so the order is deterministic across
// snapshots.
type KanbanColumn struct {
	Key   string       `json:"key"`
	Title string       `json:"title"`
	Cards []KanbanCard `json:"cards"`
}

// KanbanCard is one card in a Kanban column.
type KanbanCard struct {
	IssueID         string             `json:"issue_id"`
	IssueIdentifier string             `json:"issue_identifier"`
	Title           string             `json:"title"`
	State           string             `json:"state"`
	URL             string             `json:"url"`
	PR              *domain.PullRequest `json:"pr,omitempty"`
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
	// RunState is the operator-controlled lifecycle marker: one of
	// "running", "pause_requested", "paused", "cancel_requested". The Go
	// dashboard exposes this; the Elixir Presenter does not emit it.
	RunState string `json:"run_state"`
	// PipelineRole, PipelineCompleted, PipelineTotalRoles describe progress
	// through agent.pipeline (SPEC §10.9). Omitted on issues running under
	// the single-role legacy path.
	PipelineRole       string   `json:"pipeline_role,omitempty"`
	PipelineCompleted  []string `json:"pipeline_completed,omitempty"`
	PipelineTotalRoles int      `json:"pipeline_total_roles,omitempty"`
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

// KanbanInputs is the data BuildKanban needs to derive the column layout.
// runningIDs / retryingIDs are sets of issue identifiers currently in
// orchestrator state. specByID maps identifier → HasSpec result. prByID
// maps identifier → known PullRequest (nil/absent for issues with no PR).
// terminalStates is the configured tracker terminal-states list (lower-cased
// internally).
type KanbanInputs struct {
	AllIssues      []domain.Issue
	RunningIDs     map[string]struct{}
	RetryingIDs    map[string]struct{}
	SpecByID       map[string]bool
	PRByID         map[string]domain.PullRequest
	TerminalStates []string
}

// BuildKanban partitions issues into the five columns: Backlog (no spec),
// Ready (has spec), In Progress (running or retrying), In Review (PR
// open), Done (PR merged OR tracker state is terminal).
func BuildKanban(in KanbanInputs) []KanbanColumn {
	terminalSet := make(map[string]bool, len(in.TerminalStates))
	for _, s := range in.TerminalStates {
		terminalSet[lower(s)] = true
	}

	columns := []KanbanColumn{
		{Key: "backlog", Title: "Backlog"},
		{Key: "ready", Title: "Ready"},
		{Key: "in_progress", Title: "In Progress"},
		{Key: "in_review", Title: "In Review"},
		{Key: "done", Title: "Done"},
	}
	idx := map[string]int{
		"backlog": 0, "ready": 1, "in_progress": 2, "in_review": 3, "done": 4,
	}

	for _, issue := range in.AllIssues {
		card := KanbanCard{
			IssueID:         issue.ID,
			IssueIdentifier: issue.Identifier,
			Title:           issue.Title,
			State:           issue.State,
			URL:             issue.URL,
		}
		if pr, ok := in.PRByID[issue.Identifier]; ok {
			pcopy := pr
			card.PR = &pcopy
		}

		key := classifyKanban(issue, card.PR, in, terminalSet)
		columns[idx[key]].Cards = append(columns[idx[key]].Cards, card)
	}

	for i := range columns {
		cards := columns[i].Cards
		// stable order: identifier ascending
		sortByIdentifier(cards)
		columns[i].Cards = cards
	}
	return columns
}

func classifyKanban(issue domain.Issue, pr *domain.PullRequest, in KanbanInputs, terminalSet map[string]bool) string {
	// Done first: a merged PR or terminal state both land in Done.
	if pr != nil && pr.State == "merged" {
		return "done"
	}
	if terminalSet[lower(issue.State)] {
		return "done"
	}
	// In Review: an open PR.
	if pr != nil && pr.State == "open" {
		return "in_review"
	}
	// In Progress: running or retrying inside the orchestrator.
	if _, ok := in.RunningIDs[issue.Identifier]; ok {
		return "in_progress"
	}
	if _, ok := in.RetryingIDs[issue.Identifier]; ok {
		return "in_progress"
	}
	// Ready vs Backlog by spec presence.
	if in.SpecByID[issue.Identifier] {
		return "ready"
	}
	return "backlog"
}

func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func sortByIdentifier(cards []KanbanCard) {
	for i := 1; i < len(cards); i++ {
		for j := i; j > 0 && cards[j-1].IssueIdentifier > cards[j].IssueIdentifier; j-- {
			cards[j-1], cards[j] = cards[j], cards[j-1]
		}
	}
}
