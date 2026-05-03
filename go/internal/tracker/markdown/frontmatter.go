// Package markdown provides a filesystem-backed Tracker implementation per SPEC §11.6.
// Each .md file under tracker.markdown.root is one Issue; state lives in YAML front matter.
package markdown

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// frontMatterDelim is the YAML front-matter fence.
var frontMatterDelim = []byte("---")

// Parse splits a Markdown file's raw bytes into a parsed YAML front-matter map
// and the remaining body bytes.
//
// If the content begins with "---\n" (or "---\r\n"), the YAML between the
// opening and closing "---" delimiters is parsed; the rest is the body.
// If no front matter is present, the returned map is empty and body == content.
func Parse(content []byte) (map[string]any, []byte, error) {
	// Must start with "---" followed immediately by a newline (or \r\n).
	if !bytes.HasPrefix(content, frontMatterDelim) {
		return map[string]any{}, content, nil
	}
	rest := content[3:]
	if len(rest) == 0 || (rest[0] != '\n' && rest[0] != '\r') {
		// "---" not followed by newline — not front matter
		return map[string]any{}, content, nil
	}
	// Strip the leading newline (handle \r\n)
	if rest[0] == '\r' && len(rest) > 1 && rest[1] == '\n' {
		rest = rest[2:]
	} else {
		rest = rest[1:]
	}

	// Find the closing "---"
	idx := findClosingDelimiter(rest)
	if idx < 0 {
		// Unclosed front matter — treat whole file as body
		return map[string]any{}, content, nil
	}

	yamlBytes := rest[:idx]
	after := rest[idx+3:] // skip "---"
	// Skip the newline after the closing delimiter
	if len(after) > 0 && after[0] == '\r' && len(after) > 1 && after[1] == '\n' {
		after = after[2:]
	} else if len(after) > 0 && after[0] == '\n' {
		after = after[1:]
	}

	var fm map[string]any
	if err := yaml.Unmarshal(yamlBytes, &fm); err != nil {
		return nil, nil, fmt.Errorf("markdown: front matter YAML parse error: %w", err)
	}
	if fm == nil {
		fm = map[string]any{}
	}
	return fm, after, nil
}

// findClosingDelimiter returns the byte offset of the closing "---" line within b,
// or -1 if not found. The "---" must appear at the start of a line.
func findClosingDelimiter(b []byte) int {
	search := []byte("\n---")
	idx := bytes.Index(b, search)
	if idx < 0 {
		// Also handle "---" at the very beginning of b (edge case where yaml is empty)
		if bytes.HasPrefix(b, frontMatterDelim) {
			return 0
		}
		return -1
	}
	return idx + 1 // +1 to skip the '\n', point at '---'
}

// Serialize rebuilds a Markdown file's raw bytes from a front-matter map and body.
// If fm is empty, no front-matter block is emitted.
func Serialize(fm map[string]any, body []byte) ([]byte, error) {
	if len(fm) == 0 {
		return body, nil
	}
	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("markdown: front matter marshal error: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(yamlBytes)
	buf.WriteString("---\n")
	buf.Write(body)
	return buf.Bytes(), nil
}
