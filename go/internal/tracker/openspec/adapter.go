package openspec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// Adapter is the OpenSpec.dev filesystem tracker. Each subdirectory of
// <root>/changes/ is an active Issue; each subdirectory of <root>/archive/
// is a Done issue.
type Adapter struct {
	cfg config.Config
}

// New creates a new Adapter from the provided config.
func New(cfg config.Config) (*Adapter, error) {
	return &Adapter{cfg: cfg}, nil
}

// root returns the resolved OpenSpec root directory.
func (a *Adapter) root() string {
	return a.cfg.Tracker.OpenSpec.Root
}

// changesDir returns the path to the changes/ directory.
func (a *Adapter) changesDir() string {
	return filepath.Join(a.root(), "changes")
}

// archiveDir returns the path to the archive/ directory.
func (a *Adapter) archiveDir() string {
	return filepath.Join(a.root(), "archive")
}

// locateSlug returns (dirPath, state) for a slug by checking changes/ first,
// then archive/. Returns ("", "") if not found.
func (a *Adapter) locateSlug(slug string) (string, string) {
	changesPath := filepath.Join(a.changesDir(), slug)
	if fi, err := os.Stat(changesPath); err == nil && fi.IsDir() {
		return changesPath, "Todo"
	}
	archivePath := filepath.Join(a.archiveDir(), slug)
	if fi, err := os.Stat(archivePath); err == nil && fi.IsDir() {
		return archivePath, "Done"
	}
	return "", ""
}

// isTerminal returns true if state is in cfg.Tracker.TerminalStates.
func (a *Adapter) isTerminal(state string) bool {
	lower := strings.ToLower(state)
	for _, s := range a.cfg.Tracker.TerminalStates {
		if strings.ToLower(s) == lower {
			return true
		}
	}
	return false
}

// issueFromSlug builds a domain.Issue for the given slug located at dirPath with
// the provided state.
func (a *Adapter) issueFromSlug(slug, dirPath, state string) (domain.Issue, error) {
	proposalPath := filepath.Join(dirPath, "proposal.md")

	data, err := os.ReadFile(proposalPath)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("reading proposal.md for %s: %w", slug, err)
	}

	meta, body, err := parseFrontMatter(data)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("parsing front-matter for %s: %w", slug, err)
	}

	// Title: first H1 from body, fallback to slug.
	title := extractH1(body)
	if title == "" {
		title = slug
	}

	// Description: proposal body + tasks section from tasks.md if present.
	description := string(body)
	tasksPath := filepath.Join(dirPath, "tasks.md")
	if taskData, err := os.ReadFile(tasksPath); err == nil {
		tasksSection := extractTasksSection(taskData)
		if tasksSection != "" {
			description = description + "\n\n## Tasks\n\n" + tasksSection
		}
	}

	// Labels: ["openspec"] + any labels from front-matter.
	labels := []string{"openspec"}
	if rawLabels, ok := meta["labels"]; ok {
		switch v := rawLabels.(type) {
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					labels = append(labels, strings.ToLower(s))
				}
			}
		case []string:
			for _, s := range v {
				labels = append(labels, strings.ToLower(s))
			}
		}
	}

	// Timestamps from os.Stat of proposal.md.
	fi, err := os.Stat(proposalPath)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("stat proposal.md for %s: %w", slug, err)
	}
	modTime := fi.ModTime()

	absProposal, err := filepath.Abs(proposalPath)
	if err != nil {
		absProposal = proposalPath
	}

	return domain.Issue{
		ID:          slug,
		Identifier:  slug,
		Title:       title,
		Description: description,
		State:       state,
		URL:         "file://" + absProposal,
		Labels:      labels,
		CreatedAt:   &modTime,
		UpdatedAt:   &modTime,
	}, nil
}

// listSlugs returns all subdirectory names under dir. Returns nil if dir
// doesn't exist.
func listSlugs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			slugs = append(slugs, e.Name())
		}
	}
	return slugs, nil
}

// FetchCandidateIssues returns Issues from changes/ whose state is in
// cfg.Tracker.ActiveStates.
func (a *Adapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	activeSet := make(map[string]bool, len(a.cfg.Tracker.ActiveStates))
	for _, s := range a.cfg.Tracker.ActiveStates {
		activeSet[strings.ToLower(s)] = true
	}

	slugs, err := listSlugs(a.changesDir())
	if err != nil {
		return nil, err
	}

	var issues []domain.Issue
	for _, slug := range slugs {
		dirPath := filepath.Join(a.changesDir(), slug)
		state := "Todo"
		if !activeSet[strings.ToLower(state)] {
			continue
		}
		issue, err := a.issueFromSlug(slug, dirPath, state)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// FetchIssuesByStates returns issues matching any of the given states. When
// states includes a terminal state, archived slugs are included.
func (a *Adapter) FetchIssuesByStates(_ context.Context, states []string) ([]domain.Issue, error) {
	stateSet := make(map[string]bool, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = true
	}

	var issues []domain.Issue

	// Check changes/ directory for "Todo"-state issues.
	if stateSet["todo"] {
		slugs, err := listSlugs(a.changesDir())
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			dirPath := filepath.Join(a.changesDir(), slug)
			issue, err := a.issueFromSlug(slug, dirPath, "Todo")
			if err != nil {
				return nil, err
			}
			issues = append(issues, issue)
		}
	}

	// Check archive/ directory for terminal-state issues.
	needsArchive := false
	for _, s := range states {
		if a.isTerminal(s) {
			needsArchive = true
			break
		}
	}
	if needsArchive {
		slugs, err := listSlugs(a.archiveDir())
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			dirPath := filepath.Join(a.archiveDir(), slug)
			issue, err := a.issueFromSlug(slug, dirPath, "Done")
			if err != nil {
				return nil, err
			}
			if stateSet[strings.ToLower(issue.State)] {
				issues = append(issues, issue)
			}
		}
	}

	return issues, nil
}

// FetchIssueStatesByIDs looks up each ID (slug) in changes/ then archive/,
// returning the derived state. Slugs not found are omitted without error.
//
// Per SPEC §11.7: if changes/<slug>/.symphony-done exists, the sentinel is
// removed, the slug is auto-archived, and the returned state is "Done".
func (a *Adapter) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	var issues []domain.Issue
	for _, id := range ids {
		dirPath, state := a.locateSlug(id)
		if dirPath == "" {
			// Not found — omit per spec.
			continue
		}
		if state != "Done" {
			sentinel := filepath.Join(dirPath, ".symphony-done")
			if _, statErr := os.Stat(sentinel); statErr == nil {
				accept, reason := tasksAcceptSentinel(filepath.Join(dirPath, "tasks.md"))
				if !accept {
					slog.Warn("openspec: ignoring .symphony-done sentinel",
						"id", id, "reason", reason)
				} else {
					if err := os.Remove(sentinel); err != nil {
						return nil, fmt.Errorf("openspec: removing .symphony-done for %s: %w", id, err)
					}
					if err := a.UpdateIssueState(ctx, id, "Done"); err != nil {
						return nil, fmt.Errorf("openspec: auto-archiving %s on sentinel: %w", id, err)
					}
					dirPath = filepath.Join(a.archiveDir(), id)
					state = "Done"
				}
			}
		}
		issue, err := a.issueFromSlug(id, dirPath, state)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// CreateComment appends a comment block to the proposal.md at the slug's
// current location (changes/ or archive/).
func (a *Adapter) CreateComment(_ context.Context, issueID, body string) error {
	dirPath, _ := a.locateSlug(issueID)
	if dirPath == "" {
		return fmt.Errorf("issue %q not found in changes/ or archive/", issueID)
	}
	proposalPath := filepath.Join(dirPath, "proposal.md")
	timestamp := time.Now().UTC().Format(time.RFC3339)
	comment := fmt.Sprintf("\n## Comment %s\n%s\n", timestamp, body)

	f, err := os.OpenFile(proposalPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening proposal.md for %s: %w", issueID, err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(comment); err != nil {
		return fmt.Errorf("writing comment for %s: %w", issueID, err)
	}
	return nil
}

// CreateIssue creates a new openspec change directory with proposal.md and
// tasks.md scaffolding. Slug is derived from the title.
func (a *Adapter) CreateIssue(_ context.Context, draft domain.IssueDraft) (domain.Issue, error) {
	if strings.TrimSpace(draft.Title) == "" {
		return domain.Issue{}, fmt.Errorf("openspec: title is required")
	}
	slug := slugify(draft.Title)
	if slug == "" {
		slug = fmt.Sprintf("issue-%d", time.Now().UnixNano())
	}
	dirPath := filepath.Join(a.changesDir(), slug)
	if _, err := os.Stat(dirPath); err == nil {
		slug = fmt.Sprintf("%s-%d", slug, time.Now().Unix())
		dirPath = filepath.Join(a.changesDir(), slug)
	}

	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return domain.Issue{}, fmt.Errorf("openspec: mkdir %s: %w", dirPath, err)
	}

	proposal := buildProposalScaffold(draft)
	tasks := buildTasksScaffold(draft)
	if err := os.WriteFile(filepath.Join(dirPath, "proposal.md"), []byte(proposal), 0o644); err != nil {
		return domain.Issue{}, fmt.Errorf("openspec: write proposal.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, "tasks.md"), []byte(tasks), 0o644); err != nil {
		return domain.Issue{}, fmt.Errorf("openspec: write tasks.md: %w", err)
	}
	return a.issueFromSlug(slug, dirPath, "Todo")
}

// HasSpec returns whether <root>/changes/<slug>/proposal.md or
// <root>/archive/<slug>/proposal.md exists.
func (a *Adapter) HasSpec(_ context.Context, identifier string) (bool, error) {
	dirPath, _ := a.locateSlug(identifier)
	if dirPath == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dirPath, "proposal.md")); err == nil {
		return true, nil
	}
	return false, nil
}

// FetchAllIssues returns active (changes/) and archived (archive/) issues.
func (a *Adapter) FetchAllIssues(_ context.Context) ([]domain.Issue, error) {
	var out []domain.Issue
	for _, base := range []struct {
		dir   string
		state string
	}{
		{a.changesDir(), "Todo"},
		{a.archiveDir(), "Done"},
	} {
		slugs, err := listSlugs(base.dir)
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			issue, err := a.issueFromSlug(slug, filepath.Join(base.dir, slug), base.state)
			if err != nil {
				return nil, err
			}
			out = append(out, issue)
		}
	}
	return out, nil
}

// ReadSpec returns the proposal.md content for the given slug.
func (a *Adapter) ReadSpec(_ context.Context, identifier string) (string, string, error) {
	dirPath, _ := a.locateSlug(identifier)
	if dirPath == "" {
		return "", "", domain.ErrSpecNotFound
	}
	raw, err := os.ReadFile(filepath.Join(dirPath, "proposal.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", domain.ErrSpecNotFound
		}
		return "", "", fmt.Errorf("openspec: read proposal.md: %w", err)
	}
	body := string(raw)
	return body, etagFor(body), nil
}

// WriteSpec writes proposal.md atomically.
func (a *Adapter) WriteSpec(_ context.Context, identifier, body, ifMatchEtag string) (string, error) {
	dirPath, _ := a.locateSlug(identifier)
	if dirPath == "" {
		// Allow creating the directory on first write.
		dirPath = filepath.Join(a.changesDir(), identifier)
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			return "", fmt.Errorf("openspec: mkdir %s: %w", dirPath, err)
		}
	}
	path := filepath.Join(dirPath, "proposal.md")
	if ifMatchEtag != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("openspec: read %s: %w", path, err)
		}
		if err == nil && etagFor(string(raw)) != ifMatchEtag {
			return "", domain.ErrSpecConflict
		}
	}
	tmp, err := os.CreateTemp(dirPath, ".proposal-")
	if err != nil {
		return "", fmt.Errorf("openspec: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write([]byte(body)); err != nil {
		tmp.Close()        //nolint:errcheck
		os.Remove(tmpName) //nolint:errcheck
		return "", fmt.Errorf("openspec: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return "", fmt.Errorf("openspec: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return "", fmt.Errorf("openspec: rename: %w", err)
	}
	return etagFor(body), nil
}

// SetPullRequest is a no-op for openspec (PR data is not persisted to disk);
// the orchestrator keeps the in-memory copy.
func (a *Adapter) SetPullRequest(_ context.Context, _ string, _ domain.PullRequest) error {
	return nil
}

func buildProposalScaffold(draft domain.IssueDraft) string {
	var sb strings.Builder
	if len(draft.Labels) > 0 {
		sb.WriteString("---\nlabels:\n")
		for _, l := range draft.Labels {
			sb.WriteString("  - ")
			sb.WriteString(l)
			sb.WriteString("\n")
		}
		sb.WriteString("---\n\n")
	}
	sb.WriteString("# ")
	sb.WriteString(draft.Title)
	sb.WriteString("\n\n## Why\n\n")
	if strings.TrimSpace(draft.Description) != "" {
		sb.WriteString(strings.TrimSpace(draft.Description))
		sb.WriteString("\n")
	} else {
		sb.WriteString("_TODO_\n")
	}
	sb.WriteString("\n## What changes\n\n_TODO_\n\n## Acceptance\n\n_TODO_\n")
	return sb.String()
}

func buildTasksScaffold(_ domain.IssueDraft) string {
	return "# Tasks\n\n- [ ] _TODO_\n"
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

func etagFor(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)[:16]
}

// UpdateIssueState moves the slug's directory to the appropriate location based
// on whether the desired state is terminal. If the slug is already at the
// correct location, it is a no-op. Returns an error if the target already exists.
func (a *Adapter) UpdateIssueState(_ context.Context, issueID, state string) error {
	currentDir, _ := a.locateSlug(issueID)
	if currentDir == "" {
		return fmt.Errorf("issue %q not found", issueID)
	}

	var targetDir string
	if a.isTerminal(state) {
		targetDir = filepath.Join(a.archiveDir(), issueID)
	} else {
		targetDir = filepath.Join(a.changesDir(), issueID)
	}

	if currentDir == targetDir {
		// Already at the right location — no-op.
		return nil
	}

	// Ensure target parent directory exists.
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return fmt.Errorf("creating parent directory for %s: %w", issueID, err)
	}

	// Check that target doesn't already exist.
	if _, err := os.Stat(targetDir); err == nil {
		return fmt.Errorf("target directory %s already exists", targetDir)
	}

	if err := os.Rename(currentDir, targetDir); err != nil {
		return fmt.Errorf("moving %s to %s: %w", currentDir, targetDir, err)
	}
	return nil
}

// ---- helpers ----

// extractH1 returns the text of the first H1 heading in the markdown content,
// or "" if none is found.
func extractH1(content []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}

// tasksAcceptSentinel decides whether a `.symphony-done` sentinel should be
// honored given the contents of the change's tasks.md (if any). It returns
// (true, "") when archiving is OK, and (false, reason) when the sentinel
// should be left in place — typically because tasks.md still contains
// unchecked `- [ ]` items, meaning the agent declared completion before
// finishing its checklist. A missing or unreadable tasks.md is treated as
// "no checklist to verify" and accepts the sentinel.
func tasksAcceptSentinel(tasksPath string) (bool, string) {
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		// No tasks.md, or unreadable: nothing to verify against.
		return true, ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	unchecked := 0
	for scanner.Scan() {
		trimmed := strings.TrimLeft(scanner.Text(), " \t")
		if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "* [ ]") {
			unchecked++
		}
	}
	if unchecked > 0 {
		return false, fmt.Sprintf("tasks.md has %d unchecked item(s)", unchecked)
	}
	return true, ""
}

// extractTasksSection extracts the content under the "## Tasks" heading from
// the file content. It returns just the content lines (without the heading
// itself). If multiple H2s are present, only the "tasks" section (case-
// insensitive) is returned. Falls back to entire body if no matching H2 found.
func extractTasksSection(content []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// Find the ## Tasks heading.
	tasksStart := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			heading := strings.TrimSpace(line[3:])
			if strings.EqualFold(heading, "tasks") {
				tasksStart = i
				break
			}
		}
	}

	if tasksStart == -1 {
		// No ## Tasks heading found — nothing to extract.
		return ""
	}

	// Collect lines from tasksStart+1 until next H2 or end of file.
	var section []string
	for i := tasksStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			break
		}
		section = append(section, lines[i])
	}

	// Trim leading/trailing blank lines.
	result := strings.TrimSpace(strings.Join(section, "\n"))
	return result
}
