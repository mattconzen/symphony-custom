package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// readMaxBytes caps the byte stream returned by readTool. Exposed as a var
// (rather than a const) so tests can lower it to exercise truncation.
var readMaxBytes = 256 * 1024

// resolveInWorkspace joins a relative or absolute path with the workspace
// root and rejects any path that escapes it. The model is expected to pass
// workspace-relative paths; absolute paths under the workspace are also
// accepted because LLM outputs frequently include them.
func resolveInWorkspace(workspace, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspace, abs)
	}
	clean := filepath.Clean(abs)
	rootClean := filepath.Clean(workspace)
	if clean != rootClean && !strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace root", p)
	}
	return clean, nil
}

// readTool returns the contents of a file. Truncates beyond 256KB so the
// transport stays sane; the model has no incentive to load enormous blobs.
type readTool struct{}

func (readTool) Name() string { return "Read" }

func (readTool) Description() string {
	return "Read the contents of a file inside the workspace. Truncates files larger than 256KB."
}

func (readTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Workspace-relative file path.",
			},
		},
		"required": []string{"path"},
	}
}

func (readTool) Run(_ context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("read: invalid input: %w", err)
	}
	abs, err := resolveInWorkspace(workspacePath, in.Path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	defer f.Close() //nolint:errcheck
	// Read one byte past the cap so we can detect overflow without loading the
	// whole file into memory. Multi-GB files would OOM os.ReadFile.
	data, err := io.ReadAll(io.LimitReader(f, int64(readMaxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if len(data) > readMaxBytes {
		return string(data[:readMaxBytes]) + "\n…[truncated]\n", nil
	}
	return string(data), nil
}

// writeTool overwrites or creates a file. Caller-supplied directories are
// created on the way down.
type writeTool struct{}

func (writeTool) Name() string { return "Write" }

func (writeTool) Description() string {
	return "Create or overwrite a file inside the workspace. Parent directories are created automatically."
}

func (writeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Workspace-relative file path."},
			"content": map[string]any{"type": "string", "description": "Full file body."},
		},
		"required": []string{"path", "content"},
	}
}

func (writeTool) Run(_ context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("write: invalid input: %w", err)
	}
	abs, err := resolveInWorkspace(workspacePath, in.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("write mkdir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("write: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
}

// editTool performs an exact-string replacement, returning an error when the
// old_string is missing or non-unique (unless replace_all is set). Mirrors
// the semantics of the CLI Edit tool so workflow prompts can be shared.
type editTool struct{}

func (editTool) Name() string { return "Edit" }

func (editTool) Description() string {
	return "Replace exact text in a file. Fails when old_string is missing or non-unique unless replace_all=true."
}

func (editTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string"},
			"old_string":  map[string]any{"type": "string"},
			"new_string":  map[string]any{"type": "string"},
			"replace_all": map[string]any{"type": "boolean"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (editTool) Run(_ context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("edit: invalid input: %w", err)
	}
	abs, err := resolveInWorkspace(workspacePath, in.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	body := string(data)
	if in.ReplaceAll {
		updated := strings.ReplaceAll(body, in.OldString, in.NewString)
		if updated == body {
			return "", fmt.Errorf("edit: old_string not found")
		}
		if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil { //nolint:gosec
			return "", fmt.Errorf("edit: write: %w", err)
		}
		count := strings.Count(body, in.OldString)
		return fmt.Sprintf("replaced %d occurrence(s) in %s", count, in.Path), nil
	}
	count := strings.Count(body, in.OldString)
	if count == 0 {
		return "", fmt.Errorf("edit: old_string not found")
	}
	if count > 1 {
		return "", fmt.Errorf("edit: old_string is not unique (%d matches) — pass replace_all=true or include more context", count)
	}
	updated := strings.Replace(body, in.OldString, in.NewString, 1)
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("edit: write: %w", err)
	}
	return fmt.Sprintf("edited %s", in.Path), nil
}

// globTool walks the workspace and returns paths matching the supplied
// pattern. Filepath.Match is used for the glob; doublestar is intentionally
// not pulled in.
type globTool struct{}

func (globTool) Name() string { return "Glob" }

func (globTool) Description() string {
	return "Find files in the workspace matching a glob (e.g. **/*.go). Recursive by default."
}

func (globTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern. Supports *, ?, **."},
		},
		"required": []string{"pattern"},
	}
}

func (globTool) Run(_ context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("glob: invalid input: %w", err)
	}
	if in.Pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	pattern := filepath.ToSlash(in.Pattern)

	var matches []string
	err := filepath.WalkDir(workspacePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk
		}
		if d.IsDir() {
			// skip noisy directories aggressively to keep results tight
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(workspacePath, p)
		relSlash := filepath.ToSlash(rel)
		if doublestarMatch(pattern, relSlash) {
			matches = append(matches, rel)
		} else if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok && !strings.Contains(pattern, "/") {
			// Plain basename match when the pattern has no path separator.
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob walk: %w", err)
	}
	if len(matches) == 0 {
		return "(no matches)", nil
	}
	if len(matches) > 1000 {
		matches = matches[:1000]
		matches = append(matches, "…[truncated]")
	}
	return strings.Join(matches, "\n"), nil
}

// clipRunes returns s clipped to at most maxRunes runes, appending an ellipsis
// when truncation occurred. The rune-aware slice keeps the returned string
// valid UTF-8 even when the cut would have split a multibyte sequence.
func clipRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// doublestarMatch reports whether `name` matches `pattern`, where `**` matches
// zero or more path components, `*` matches anything except `/`, and `?`
// matches one non-`/` byte. Equivalent to github.com/bmatcuk/doublestar/v4
// for the subset of patterns globTool exposes.
func doublestarMatch(pattern, name string) bool {
	patParts := splitPath(pattern)
	nameParts := splitPath(name)
	return matchParts(patParts, nameParts)
}

func splitPath(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trim consecutive ** to avoid exponential branching.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				// Trailing **: matches any remaining components, including none.
				return true
			}
			// Try matching the remainder against every suffix of name.
			for i := 0; i <= len(name); i++ {
				if matchParts(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, _ := filepath.Match(pat[0], name[0])
		if !ok {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// grepTool searches file contents with a regex. Implementation reads each
// file line-by-line; intentionally simple — no ripgrep, no goroutines.
type grepTool struct{}

func (grepTool) Name() string { return "Grep" }

func (grepTool) Description() string {
	return "Search workspace files line-by-line for a regex. Returns 'path:line:match' rows, capped at 500 hits."
}

func (grepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Go regexp pattern."},
			"path":    map[string]any{"type": "string", "description": "Optional workspace-relative root to limit search."},
		},
		"required": []string{"pattern"},
	}
}

func (grepTool) Run(_ context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("grep: invalid input: %w", err)
	}
	if in.Pattern == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid regex: %w", err)
	}
	root := workspacePath
	if in.Path != "" {
		root, err = resolveInWorkspace(workspacePath, in.Path)
		if err != nil {
			return "", err
		}
	}

	var hits []string
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= 500 {
			return filepath.SkipAll
		}
		f, err := os.Open(p) //nolint:gosec
		if err != nil {
			return nil //nolint:nilerr
		}
		defer f.Close() //nolint:errcheck
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		ln := 0
		rel, _ := filepath.Rel(workspacePath, p)
		for scanner.Scan() {
			ln++
			line := scanner.Text()
			if re.MatchString(line) {
				line = clipRunes(line, 200)
				hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, ln, line))
				if len(hits) >= 500 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("grep walk: %w", walkErr)
	}
	if len(hits) == 0 {
		return "(no matches)", nil
	}
	return strings.Join(hits, "\n"), nil
}
