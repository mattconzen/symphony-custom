package web

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/workspace"
)

const specGenPromptTemplate = `You are drafting an OpenSpec proposal.md for the following work item.

Title: {{title}}

Description:
{{description}}

Output format: a single Markdown document with these sections, in this order:

# {{title}}

## Why

(1-3 paragraphs explaining the motivation)

## What changes

(bulleted or numbered list of concrete changes)

## Acceptance

(verifiable acceptance criteria, ideally as a checklist)

Output ONLY the Markdown document. No preamble, no commentary.`

// runtimeSpecGenerator drafts spec content by invoking the configured agent
// runtime in a throwaway workspace.
type runtimeSpecGenerator struct {
	rt   agent.Runtime
	cfg  config.Config
	ws   *workspace.Manager
	log  *observability.Logger
	mu   sync.Mutex
}

// NewRuntimeSpecGenerator wires the spec endpoint to the configured agent
// runtime. The runtime is invoked synchronously (per-request), one call at a
// time (mu serializes), with the issue title + description rendered into a
// hard-coded prompt template.
func NewRuntimeSpecGenerator(rt agent.Runtime, cfg config.Config, ws *workspace.Manager, log *observability.Logger) SpecGenerator {
	return &runtimeSpecGenerator{rt: rt, cfg: cfg, ws: ws, log: log}
}

// Generate runs one agent turn against a workspace ensured for the issue.
// Returns the assistant's last text payload.
func (g *runtimeSpecGenerator) Generate(ctx context.Context, issue domain.Issue) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	prompt := strings.ReplaceAll(specGenPromptTemplate, "{{title}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{description}}", issue.Description)

	wsd, err := g.ws.EnsureForIssue(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("specgen: ensure workspace: %w", err)
	}
	sess, err := g.rt.StartSession(ctx, wsd)
	if err != nil {
		return "", fmt.Errorf("specgen: start session: %w", err)
	}
	defer func() { _ = g.rt.StopSession(ctx, sess) }() //nolint:errcheck

	var buf strings.Builder
	cb := func(ev agent.Event) {
		if ev.Kind == agent.EventAssistantMessage {
			fmt.Fprintf(&buf, "%v\n", ev.Payload)
		}
	}
	if _, err := g.rt.RunTurn(ctx, sess, prompt, issue, cb); err != nil {
		return "", fmt.Errorf("specgen: run turn: %w", err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "", fmt.Errorf("specgen: agent returned empty output")
	}
	return out, nil
}
