// Package domain contains the core domain types for Symphony.
package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// ErrCreateUnsupported is returned by Tracker.CreateIssue on read-only
// trackers (Linear, JIRA).
var ErrCreateUnsupported = errors.New("tracker: create not supported by this tracker")

// ErrSpecNotFound is returned by SpecReader.ReadSpec when no spec exists for
// the identifier.
var ErrSpecNotFound = errors.New("tracker: spec not found")

// ErrSpecConflict is returned by SpecWriter.WriteSpec when the supplied
// ifMatchEtag does not match the current spec's etag.
var ErrSpecConflict = errors.New("tracker: spec conflict (stale etag)")

// IssueDraft is the input to Tracker.CreateIssue. Adapters that need a
// filesystem slug derive it from Title.
type IssueDraft struct {
	Title       string
	Description string
	Labels      []string
}

// nonWorkspaceKey matches characters not allowed in workspace keys.
var nonWorkspaceKey = regexp.MustCompile(`[^A-Za-z0-9._\-]`)

// BlockerRef is a reference to a blocker issue.
type BlockerRef struct {
	ID         string
	Identifier string
	State      string
}

// PullRequest captures the GitHub pull request associated with an issue.
// Populated by agent-emitted pr_link events and by the GitHub PR reconciler.
type PullRequest struct {
	URL       string     `json:"url"`
	Number    int        `json:"number"`
	Owner     string     `json:"owner"`
	Repo      string     `json:"repo"`
	State     string     `json:"state"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	Source    string     `json:"source"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// PipelineProgress tracks per-role completion when an issue is being run
// through agent.pipeline (SPEC §10.9). Nil on issues that run under the
// single-role legacy path.
type PipelineProgress struct {
	CurrentRole    string            `json:"current_role,omitempty"`
	CompletedRoles []string          `json:"completed_roles,omitempty"`
	Loopbacks      map[string]int    `json:"loopbacks,omitempty"`
	Artifacts      map[string]string `json:"artifacts,omitempty"`
}

// Issue is the normalized issue record used by orchestration, prompt rendering,
// and observability.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	Priority    *int
	State       string
	BranchName  string
	URL         string
	Labels      []string
	BlockedBy   []BlockerRef
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
	PR          *PullRequest
	Pipeline    *PipelineProgress
}

// WorkspaceKey returns the sanitized workspace directory name derived from the
// issue identifier per SPEC §4.2: replace any character not in [A-Za-z0-9._-]
// with '_'.
func (i Issue) WorkspaceKey() string {
	return nonWorkspaceKey.ReplaceAllString(i.Identifier, "_")
}

// NormalizedState returns the lowercased state for comparison per SPEC §4.2.
func (i Issue) NormalizedState() string {
	return strings.ToLower(i.State)
}
