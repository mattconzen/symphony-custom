// Package prompt renders Liquid templates for issue prompts per SPEC §5.4.
package prompt

import (
	"errors"
	"fmt"
	"strings"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/osteele/liquid"
)

// ErrTemplateRender is returned when rendering fails due to unknown variables
// or filters.
var ErrTemplateRender = errors.New("template_render_error")

// Vars holds the template input variables per SPEC §5.4.
type Vars struct {
	Issue   domain.Issue
	Attempt *int
}

// Render renders a Liquid template with the given vars per SPEC §5.4.
// Unknown variables cause a render error (strict mode).
func Render(template string, vars Vars) (string, error) {
	if template == "" {
		return "", nil
	}

	// Build the bindings map for the Liquid engine.
	issueMap := issueToMap(vars.Issue)

	bindings := map[string]any{
		"issue": issueMap,
	}
	if vars.Attempt != nil {
		bindings["attempt"] = *vars.Attempt
	}

	// Collect variable names referenced in the template to detect unknowns.
	// osteele/liquid by default renders unknown variables as empty string.
	// We implement strict mode by checking which variables are referenced.
	knownTopLevel := map[string]bool{
		"issue":   true,
		"attempt": true,
	}

	// Extract top-level variable references from the template.
	// A naive approach: scan for {{ varname... }} patterns.
	referenced := extractTopLevelVars(template)
	for _, ref := range referenced {
		if !knownTopLevel[ref] {
			return "", fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, ref)
		}
	}

	engine := liquid.NewEngine()
	out, err := engine.ParseAndRenderString(template, bindings)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTemplateRender, err)
	}

	return out, nil
}

// extractTopLevelVars returns the top-level variable names referenced in
// Liquid template expressions (e.g. "issue" from "{{ issue.identifier }}").
func extractTopLevelVars(template string) []string {
	var refs []string
	seen := map[string]bool{}

	i := 0
	for i < len(template) {
		start := strings.Index(template[i:], "{{")
		if start == -1 {
			break
		}
		start += i
		end := strings.Index(template[start:], "}}")
		if end == -1 {
			break
		}
		end += start + 2

		expr := strings.TrimSpace(template[start+2 : end-2])
		// Extract the first identifier (before any . or | or space)
		ident := firstIdent(expr)
		if ident != "" && !seen[ident] {
			seen[ident] = true
			refs = append(refs, ident)
		}

		i = end
	}
	return refs
}

// firstIdent extracts the leading identifier from an expression string.
func firstIdent(expr string) string {
	expr = strings.TrimSpace(expr)
	for idx, ch := range expr {
		if ch == '.' || ch == '|' || ch == ' ' || ch == '\t' {
			return expr[:idx]
		}
	}
	return expr
}

// issueToMap converts an Issue to a map[string]any for template rendering.
func issueToMap(issue domain.Issue) map[string]any {
	m := map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": issue.Description,
		"state":       issue.State,
		"branch_name": issue.BranchName,
		"url":         issue.URL,
		"labels":      issue.Labels,
	}
	if issue.Priority != nil {
		m["priority"] = *issue.Priority
	} else {
		m["priority"] = nil
	}
	if issue.CreatedAt != nil {
		m["created_at"] = issue.CreatedAt.String()
	}
	if issue.UpdatedAt != nil {
		m["updated_at"] = issue.UpdatedAt.String()
	}
	// Blockers
	blockers := make([]map[string]any, 0, len(issue.BlockedBy))
	for _, b := range issue.BlockedBy {
		blockers = append(blockers, map[string]any{
			"id":         b.ID,
			"identifier": b.Identifier,
			"state":      b.State,
		})
	}
	m["blocked_by"] = blockers
	return m
}
