// Package openspec provides a filesystem-based Tracker implementation where each
// openspec/changes/<slug>/ directory represents one active Issue per SPEC §5.3.9.
package openspec

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseFrontMatter splits a markdown document into YAML front-matter metadata
// and the body. The front-matter block is delimited by leading "---" lines.
// If no front-matter is present, meta is empty and body is the full content.
func parseFrontMatter(content []byte) (meta map[string]any, body []byte, err error) {
	s := string(content)

	if !strings.HasPrefix(s, "---") {
		return map[string]any{}, content, nil
	}

	// Strip opening "---"
	rest := s[3:]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	} else if len(rest) > 1 && rest[0] == '\r' && rest[1] == '\n' {
		rest = rest[2:]
	}

	// Find closing "---"
	end := strings.Index(rest, "\n---")
	if end == -1 {
		// No closing delimiter — treat entire content as body.
		return map[string]any{}, content, nil
	}

	frontRaw := rest[:end]
	bodyStr := rest[end+4:] // skip "\n---"
	if len(bodyStr) > 0 && bodyStr[0] == '\n' {
		bodyStr = bodyStr[1:]
	} else if len(bodyStr) > 1 && bodyStr[0] == '\r' && bodyStr[1] == '\n' {
		bodyStr = bodyStr[2:]
	}

	var raw any
	if err := yaml.Unmarshal([]byte(frontRaw), &raw); err != nil {
		return nil, nil, fmt.Errorf("parsing front-matter YAML: %w", err)
	}

	if raw == nil {
		return map[string]any{}, []byte(bodyStr), nil
	}

	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("front-matter must be a YAML mapping, got %T", raw)
	}

	return m, []byte(bodyStr), nil
}
