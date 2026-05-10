package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is one executable capability exposed to the Anthropic model. Each
// returns a string body that is wrapped in a tool_result content block on
// the next request.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Run(ctx context.Context, workspacePath string, input json.RawMessage) (string, error)
}

// toolRegistry indexes Tools by name. Lookup is case-sensitive; the model
// must call tools by the exact registered name.
type toolRegistry struct {
	byName map[string]Tool
	order  []string
}

func newToolRegistry(tools ...Tool) *toolRegistry {
	r := &toolRegistry{byName: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		r.byName[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	return r
}

func (r *toolRegistry) lookup(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// defs returns the toolDef slice the Messages API expects.
func (r *toolRegistry) defs() []toolDef {
	defs := make([]toolDef, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		defs = append(defs, toolDef{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return defs
}

// runTool executes the named tool. Errors are returned as tool_result bodies
// with IsError=true rather than propagating to the caller; the model needs
// to see the failure to recover.
func (r *toolRegistry) runTool(ctx context.Context, workspacePath, name string, input json.RawMessage) (string, bool) {
	t, ok := r.lookup(name)
	if !ok {
		return fmt.Sprintf("tool %q not found", name), true
	}
	out, err := t.Run(ctx, workspacePath, input)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	// Anthropic rejects empty-string tool_result content with a 400. Substitute
	// a placeholder so silent-success tools (e.g. a no-op bash) don't poison
	// the next request.
	if out == "" {
		out = "(no output)"
	}
	return out, false
}

// defaultTools returns the canonical Symphony tool set.
func defaultTools() []Tool {
	return []Tool{
		bashTool{},
		readTool{},
		writeTool{},
		editTool{},
		globTool{},
		grepTool{},
	}
}
