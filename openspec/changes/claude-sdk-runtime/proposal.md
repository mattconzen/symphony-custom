---
labels:
  - runtime
  - claude
  - future-work
---

# Add `claude_sdk` agent runtime for true persistent multi-turn

## Why

The current `claude` runtime in `go/internal/agent/claude/` is single-turn-only because the Claude
CLI (`claude --print`) exits after emitting one `result` event. SPEC §10.8.7 codifies this as a
preflight constraint: `agent.runtime=claude` MUST have `agent.max_turns=1`. This works, but it
diverges from Codex's persistent-subprocess model (SPEC §10.3) and forecloses Symphony's continuation
and `between_turns` features for Claude users.

The Claude CLI has no persistent stdio mode. Empirically:

- `--output-format stream-json` and `--input-format stream-json` only work with `--print`, which
  per its CLI help "prints response and exits."
- Removing `--print` disables stream-json entirely.
- There is no `claude app-server` subcommand analogous to `codex app-server`.

The only way to get true persistent multi-turn against Claude is to skip the CLI and call the
Anthropic API directly via the official Go SDK (`github.com/anthropics/anthropic-sdk-go`). The SDK
provides a thin wrapper over the Messages API; multi-turn is a matter of appending each
`message.ToParam()` to a `[]anthropic.MessageParam` slice and resending.

## What changes

A new `agent.runtime: claude_sdk` value, selected via `cfg.Agent.Runtime`, with its own
implementation under `go/internal/agent/claudesdk/`. The existing `claude` runtime (CLI-based,
single-turn) stays — operators choose based on whether they need multi-turn or want the
batteries-included CLI agent loop.

Required additions:

- **Runtime package** implementing `agent.Runtime`, with persistent in-process state across
  `RunTurn` calls (no subprocess at all).
- **Tool implementations** for the standard Symphony toolset. The Anthropic SDK gives you
  `ToolUseBlock` / `ToolResultBlock` plumbing but does not execute tools. At minimum:
  - `Bash` (shell execution sandboxed to the workspace)
  - `Edit`, `Read`, `Write`, `Glob`, `Grep` (filesystem ops)
  - `WebFetch` (optional; consider security implications)
- **Permission flow.** The CLI's `--permission-mode` (`acceptEdits`, `bypassPermissions`,
  `plan`, etc.) has no SDK equivalent — Symphony has to implement its own approval policy or
  accept that this runtime always runs in `bypassPermissions` mode (workspaces are intentionally
  scoped per SPEC §9, so this is defensible for a sandboxed orchestrator).
- **MCP support** (deferred). The CLI loads `.mcp.json` and project-level MCP servers
  automatically; the SDK does not. Initial scope skips MCP and documents the gap.
- **Conversation history serialization.** The orchestrator already exposes
  `cfg.Agent.MaxTurns` and a continuation prompt; the SDK runtime stitches the conversation by
  keeping `[]anthropic.MessageParam` in `agent.Session.Impl`.
- **New `claude_sdk` config block** in `internal/config/config.go` mirroring the existing
  `claude` block: `model`, `api_key_env` (default `ANTHROPIC_API_KEY`), `max_tokens`,
  `turn_timeout_ms`, etc.
- **SPEC additions.** A new §10.9 specifying the wire protocol (HTTP Messages API), tool
  contracts, and the canonical Symphony tool set.
- **Preflight rejection** of `claude_sdk` + `max_turns > 1` is **lifted** for this runtime —
  multi-turn is the whole point.

## Non-goals

- Replacing the existing `claude` (CLI) runtime. They coexist.
- Reimplementing the Claude Code agent's full feature set (sub-agents, hooks, plan mode,
  custom commands, `.claude/CLAUDE.md` loading). Symphony's tool surface stays minimal.
- Cross-runtime compatibility. A Symphony workflow that works under `claude` may need
  prompt-template adjustments under `claude_sdk` because the SDK runtime won't have access to
  CLI-specific tools (e.g. the CLI's `WebSearch`, `TodoWrite`).

## Acceptance

- `go test ./internal/agent/claudesdk/...` passes, including a contract test mirroring
  `internal/agent/contract_test.go` with multi-turn coverage.
- The contract test in `internal/agent/contract_test.go` is parameterized over
  `[mock, codex, claude, claude_sdk]`.
- `agent.runtime: claude_sdk` with `agent.max_turns: 5` runs end-to-end against an OpenSpec
  change in a manual smoke test.
- SPEC §10.9 is added, normative, and reviewed.
- The existing `claude` (CLI) runtime is untouched.

## Out of scope (file as separate work if desired)

- Streaming responses from the SDK (use `client.Messages.NewStreaming`). Initial implementation
  can use blocking `client.Messages.New` since Symphony already buffers events for orchestration.
- A `claude_sdk` smoke test in `e2e/` requiring a real `ANTHROPIC_API_KEY`. Gate behind
  `SYMPHONY_RUN_LIVE_CLAUDE_SDK=1` like the existing live tests.
