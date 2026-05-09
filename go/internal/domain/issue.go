// Package domain contains the core domain types for Symphony.
package domain

import (
	"regexp"
	"strings"
	"time"
)

// nonWorkspaceKey matches characters not allowed in workspace keys.
var nonWorkspaceKey = regexp.MustCompile(`[^A-Za-z0-9._\-]`)

// BlockerRef is a reference to a blocker issue.
type BlockerRef struct {
	ID         string
	Identifier string
	State      string
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
