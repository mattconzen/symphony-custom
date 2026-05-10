package markdown

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// Adapter implements tracker.Tracker backed by a directory of Markdown files
// per SPEC §11.6. Each .md file is one Issue; state lives in YAML front matter.
type Adapter struct {
	cfg config.Config
}

// New creates a new Adapter. Returns an error if tracker.markdown.root is empty.
func New(cfg config.Config) (*Adapter, error) {
	if cfg.Tracker.Markdown.Root == "" {
		return nil, fmt.Errorf("markdown: tracker.markdown.root is required")
	}
	return &Adapter{cfg: cfg}, nil
}

// FetchCandidateIssues returns all Issues whose state is in cfg.Tracker.ActiveStates
// (case-insensitive) per SPEC §11.6.5.
func (a *Adapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return a.fetchByStates(a.cfg.Tracker.ActiveStates)
}

// FetchIssuesByStates returns all Issues whose state is in the supplied list
// (case-insensitive).
func (a *Adapter) FetchIssuesByStates(_ context.Context, states []string) ([]domain.Issue, error) {
	return a.fetchByStates(states)
}

// FetchIssueStatesByIDs re-reads the filesystem and returns matching Issues.
// The filesystem is the source of truth per SPEC §11.6.
func (a *Adapter) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]domain.Issue, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}

	var out []domain.Issue
	err := a.walk(func(issue domain.Issue) error {
		if wanted[issue.ID] {
			out = append(out, issue)
		}
		return nil
	})
	return out, err
}

// CreateComment appends a timestamped comment section to the Issue's file per
// SPEC §11.6.3. The write is atomic (tmp + rename).
func (a *Adapter) CreateComment(_ context.Context, issueID, body string) error {
	path, err := a.findByID(issueID)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("markdown: read %s: %w", path, err)
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	comment := fmt.Sprintf("\n## Comment %s\n\n%s\n", ts, body)

	updated := append(existing, []byte(comment)...) //nolint:gocritic
	return atomicWrite(path, updated)
}

// CreateIssue writes a new <root>/<slug>.md with YAML front matter and the
// supplied description as the body. Slug is derived from the title.
func (a *Adapter) CreateIssue(_ context.Context, draft domain.IssueDraft) (domain.Issue, error) {
	root := a.cfg.Tracker.Markdown.Root
	if root == "" {
		return domain.Issue{}, fmt.Errorf("markdown: tracker.markdown.root not configured")
	}
	if strings.TrimSpace(draft.Title) == "" {
		return domain.Issue{}, fmt.Errorf("markdown: title is required")
	}

	slug := slugify(draft.Title)
	if slug == "" {
		slug = fmt.Sprintf("issue-%d", time.Now().UnixNano())
	}
	path := filepath.Join(root, slug+".md")
	if _, err := os.Stat(path); err == nil {
		// Avoid clobbering an existing file by appending a timestamp suffix.
		slug = fmt.Sprintf("%s-%d", slug, time.Now().Unix())
		path = filepath.Join(root, slug+".md")
	}

	now := time.Now().UTC()
	state := "Todo"
	if len(a.cfg.Tracker.ActiveStates) > 0 {
		state = a.cfg.Tracker.ActiveStates[0]
	}
	fm := map[string]any{
		"state":      state,
		"created_at": now.Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	}
	if len(draft.Labels) > 0 {
		fm["labels"] = draft.Labels
	}

	body := fmt.Sprintf("# %s\n\n%s\n", draft.Title, strings.TrimRight(draft.Description, "\n"))
	out, err := Serialize(fm, []byte(body))
	if err != nil {
		return domain.Issue{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return domain.Issue{}, fmt.Errorf("markdown: mkdir %s: %w", root, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return domain.Issue{}, fmt.Errorf("markdown: write %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return domain.Issue{
		ID:          fileID(abs),
		Identifier:  slug,
		Title:       draft.Title,
		Description: strings.TrimSpace(body),
		State:       state,
		URL:         "file://" + abs,
		Labels:      draft.Labels,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}, nil
}

// HasSpec reports whether the markdown issue has an OpenSpec-style spec.
// True iff its front-matter has openspec: true OR a sibling <slug>.spec.md
// exists alongside the issue file.
func (a *Adapter) HasSpec(_ context.Context, identifier string) (bool, error) {
	root := a.cfg.Tracker.Markdown.Root
	mdPath := filepath.Join(root, identifier+".md")
	specPath := filepath.Join(root, identifier+".spec.md")

	if _, err := os.Stat(specPath); err == nil {
		return true, nil
	}

	raw, err := os.ReadFile(mdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("markdown: read %s: %w", mdPath, err)
	}
	fm, _, err := Parse(raw)
	if err != nil {
		return false, nil // malformed front-matter -> no spec
	}
	if v, ok := fm["openspec"]; ok {
		if b, ok := v.(bool); ok {
			return b, nil
		}
	}
	return false, nil
}

// FetchAllIssues lists every .md file under the markdown root.
func (a *Adapter) FetchAllIssues(_ context.Context) ([]domain.Issue, error) {
	var out []domain.Issue
	err := a.walk(func(issue domain.Issue) error {
		out = append(out, issue)
		return nil
	})
	return out, err
}

// ReadSpec returns the body of <root>/<identifier>.spec.md if it exists.
func (a *Adapter) ReadSpec(_ context.Context, identifier string) (string, string, error) {
	specPath := filepath.Join(a.cfg.Tracker.Markdown.Root, identifier+".spec.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", domain.ErrSpecNotFound
		}
		return "", "", fmt.Errorf("markdown: read %s: %w", specPath, err)
	}
	body := string(raw)
	return body, etagFor(body), nil
}

// WriteSpec writes <root>/<identifier>.spec.md atomically.
func (a *Adapter) WriteSpec(_ context.Context, identifier, body, ifMatchEtag string) (string, error) {
	specPath := filepath.Join(a.cfg.Tracker.Markdown.Root, identifier+".spec.md")
	if ifMatchEtag != "" {
		raw, err := os.ReadFile(specPath)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("markdown: read %s: %w", specPath, err)
		}
		if err == nil && etagFor(string(raw)) != ifMatchEtag {
			return "", domain.ErrSpecConflict
		}
	}
	if err := atomicWrite(specPath, []byte(body)); err != nil {
		return "", err
	}
	return etagFor(body), nil
}

// etagFor returns a short content-derived etag for a spec body.
func etagFor(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)[:16]
}

// slugify lowercases the input and replaces non-alphanumerics with dashes.
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

// UpdateIssueState rewrites the front-matter state: key atomically per SPEC §11.6.4.
func (a *Adapter) UpdateIssueState(_ context.Context, issueID, state string) error {
	path, err := a.findByID(issueID)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("markdown: read %s: %w", path, err)
	}

	fm, body, err := Parse(raw)
	if err != nil {
		return err
	}

	fm["state"] = state

	updated, err := Serialize(fm, body)
	if err != nil {
		return err
	}
	return atomicWrite(path, updated)
}

// ---- internal helpers ----

// fetchByStates walks the root and returns issues whose state matches any of
// the supplied states (case-insensitive).
func (a *Adapter) fetchByStates(states []string) ([]domain.Issue, error) {
	stateSet := make(map[string]bool, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = true
	}

	var out []domain.Issue
	err := a.walk(func(issue domain.Issue) error {
		if stateSet[strings.ToLower(issue.State)] {
			out = append(out, issue)
		}
		return nil
	})
	return out, err
}

// walk enumerates all .md files under Markdown.Root (recursive, skipping
// dot-files/dot-dirs) and calls fn for each parsed Issue.
func (a *Adapter) walk(fn func(domain.Issue) error) error {
	root := a.cfg.Tracker.Markdown.Root
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip dot-files and dot-directories
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			return nil
		}

		issue, err := fileToIssue(path)
		if err != nil {
			return err
		}
		return fn(issue)
	})
}

// findByID walks the root to find the file whose SHA-256 ID matches issueID.
func (a *Adapter) findByID(issueID string) (string, error) {
	var found string
	err := a.walk(func(issue domain.Issue) error {
		if issue.ID == issueID {
			found = issue.URL[len("file://"):] // strip scheme to recover absolute path
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("markdown: issue %q not found", issueID)
	}
	return found, nil
}

// errStop is a sentinel used to short-circuit WalkDir when a match is found.
var errStop = fmt.Errorf("stop")

// fileToIssue parses a single .md file into a domain.Issue per SPEC §11.6.2.
func fileToIssue(path string) (domain.Issue, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("markdown: abs path for %s: %w", path, err)
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		return domain.Issue{}, fmt.Errorf("markdown: read %s: %w", absPath, err)
	}

	fm, body, err := Parse(raw)
	if err != nil {
		return domain.Issue{}, err
	}

	stem := strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	title := extractH1(body)
	if title == "" {
		title = stem
	}

	issue := domain.Issue{
		ID:          fileID(absPath),
		Identifier:  stem,
		URL:         "file://" + absPath,
		Title:       title,
		Description: strings.TrimSpace(string(body)),
		State:       fmState(fm),
		Priority:    fmPriority(fm),
		Labels:      fmLabels(fm),
	}

	applyTimestamps(&issue, fm, absPath)
	return issue, nil
}

// fmState returns the state from front-matter, defaulting to "Todo".
func fmState(fm map[string]any) string {
	if s, ok := fm["state"].(string); ok && s != "" {
		return s
	}
	return "Todo"
}

// fmPriority returns a pointer to the priority int from front-matter, or nil.
func fmPriority(fm map[string]any) *int {
	if p, ok := fm["priority"]; ok {
		if n, ok := toInt(p); ok {
			v := n
			return &v
		}
	}
	return nil
}

// fmLabels returns normalized (lowercased) labels from front-matter.
func fmLabels(fm map[string]any) []string {
	if l, ok := fm["labels"]; ok {
		return normalizeLabels(l)
	}
	return nil
}

// applyTimestamps sets CreatedAt and UpdatedAt on issue from front-matter or file mtime.
func applyTimestamps(issue *domain.Issue, fm map[string]any, absPath string) {
	mt := fileMtime(absPath)
	issue.CreatedAt = fmTime(fm, "created_at")
	if issue.CreatedAt == nil && !mt.IsZero() {
		issue.CreatedAt = &mt
	}
	issue.UpdatedAt = fmTime(fm, "updated_at")
	if issue.UpdatedAt == nil && !mt.IsZero() {
		issue.UpdatedAt = &mt
	}
}

// fileMtime returns the file's modification time in UTC, or zero time on error.
func fileMtime(absPath string) time.Time {
	info, err := os.Stat(absPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

// fmTime parses an RFC3339 timestamp from a front-matter key, returning nil on absence/error.
func fmTime(fm map[string]any, key string) *time.Time {
	if s, ok := fm[key].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return &t
		}
	}
	return nil
}

// fileID returns the SHA-256 hex digest of the absolute file path per SPEC §11.6.2.
func fileID(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h)
}

// extractH1 returns the text of the first `# Heading` in the body, or "".
func extractH1(body []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}

// normalizeLabels converts a raw YAML value to a lowercased []string.
func normalizeLabels(v any) []string {
	switch raw := v.(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, strings.ToLower(s))
			}
		}
		return out
	case []string:
		out := make([]string, len(raw))
		for i, s := range raw {
			out[i] = strings.ToLower(s)
		}
		return out
	}
	return nil
}

// atomicWrite writes content to path via a tmp file + rename.
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mdtmp-")
	if err != nil {
		return fmt.Errorf("markdown: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()        //nolint:errcheck
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("markdown: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("markdown: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("markdown: rename temp to %s: %w", path, err)
	}
	return nil
}

// toInt converts a yaml.v3 numeric value (int, int64, float64) to int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
