// Package workflow loads and parses WORKFLOW.md files per SPEC §5.2.
package workflow

import (
	"errors"
	"os"
	"strings"

	"github.com/openai/symphony/go/internal/domain"
	"gopkg.in/yaml.v3"
)

// Sentinel errors per SPEC §5.5.
var (
	// ErrMissingWorkflowFile is returned when the workflow file cannot be read.
	ErrMissingWorkflowFile = errors.New("missing_workflow_file")
	// ErrFrontMatterNotMap is returned when YAML front matter does not decode to
	// a map/object.
	ErrFrontMatterNotMap = errors.New("workflow_front_matter_not_a_map")
)

// Load reads and parses a WORKFLOW.md file per SPEC §5.2.
//
// If the file starts with "---", lines until the next "---" are parsed as YAML
// front matter. The remaining lines become the prompt body (trimmed).
//
// If front matter is absent, the entire file is treated as prompt body and an
// empty config map is returned.
//
// YAML front matter MUST decode to a map; non-map YAML returns ErrFrontMatterNotMap.
func Load(path string) (domain.Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Workflow{}, ErrMissingWorkflowFile
	}

	content := string(data)

	if !strings.HasPrefix(content, "---") {
		// No front matter – whole file is the prompt body.
		return domain.Workflow{
			Config:         map[string]any{},
			PromptTemplate: strings.TrimSpace(content),
		}, nil
	}

	// Strip the opening "---\n" (or "---\r\n").
	rest := content[3:]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	} else if len(rest) > 1 && rest[0] == '\r' && rest[1] == '\n' {
		rest = rest[2:]
	}

	// Find the closing "---".
	end := strings.Index(rest, "\n---")
	if end == -1 {
		// Malformed: no closing delimiter – treat entire content as body.
		return domain.Workflow{
			Config:         map[string]any{},
			PromptTemplate: strings.TrimSpace(content),
		}, nil
	}

	frontMatterRaw := rest[:end]
	// Body starts after the closing "---" line.
	body := rest[end+4:] // skip "\n---"
	// Skip optional newline after closing delimiter.
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	} else if len(body) > 1 && body[0] == '\r' && body[1] == '\n' {
		body = body[2:]
	}

	var raw any
	if err := yaml.Unmarshal([]byte(frontMatterRaw), &raw); err != nil {
		return domain.Workflow{}, err
	}

	if raw == nil {
		return domain.Workflow{
			Config:         map[string]any{},
			PromptTemplate: strings.TrimSpace(body),
		}, nil
	}

	configMap, ok := raw.(map[string]any)
	if !ok {
		return domain.Workflow{}, ErrFrontMatterNotMap
	}

	return domain.Workflow{
		Config:         configMap,
		PromptTemplate: strings.TrimSpace(body),
	}, nil
}
