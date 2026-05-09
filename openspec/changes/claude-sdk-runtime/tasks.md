# Tasks

## Design

- [ ] Decide tool set for the SDK runtime: minimum (Bash + filesystem) vs. closer-to-CLI parity.
- [ ] Decide whether to ship MCP support in v1 or defer.
- [ ] Decide permission-flow model: always-bypass (relying on workspace sandbox) vs. per-tool
      approval hook.
- [ ] Pin the Anthropic Go SDK version in `go.mod` and document the upgrade policy.

## Implementation

- [ ] Add `internal/agent/claudesdk/` package skeleton implementing `agent.Runtime`.
- [ ] Implement `StartSession` returning `agent.Session{Impl: &sdkSession{messages: nil, client: ...}}`.
- [ ] Implement `RunTurn`: append user message → `client.Messages.New` → walk content blocks,
      execute `ToolUseBlock`s via the runtime's tool registry → append `ToolResultBlock`s → loop
      until no more tool calls → return `agent.TurnResult`.
- [ ] Implement `StopSession` (no subprocess to kill — just mark session closed).
- [ ] Implement Bash tool with workspace-scoped exec.
- [ ] Implement Edit / Read / Write / Glob / Grep tools.
- [ ] Add `cfg.ClaudeSDK` config struct + Resolve defaults + Preflight validation.
- [ ] Register the runtime via `agent.RegisterClaudeSDKFactory` (or extend the existing factory
      pattern in `agent/agent.go`).

## Tests

- [ ] Unit tests for each tool implementation under `internal/agent/claudesdk/tools/`.
- [ ] Mock the SDK's HTTP client (the SDK supports `option.WithHTTPClient`) for hermetic
      multi-turn tests.
- [ ] Add `claude_sdk` to the `runtimeCases` in `internal/agent/contract_test.go`. Parameterize
      the contract suite over multi-turn scenarios.
- [ ] Add a multi-turn happy-path test exercising 3+ turns with tool calls in between.
- [ ] Add a live-API test gated behind `SYMPHONY_RUN_LIVE_CLAUDE_SDK=1`.

## Spec

- [ ] Draft `go/SPEC.md` §10.9 covering: launch contract (none — in-process), session/turn IDs,
      event mapping (synthetic — derive from SDK responses), tool contracts, timeouts.
- [ ] Update §6.4 cheat sheet with the new `claude_sdk.*` config keys.
- [ ] Update §10.8.7 to clarify the constraint applies to `claude` only, not `claude_sdk`.

## Cleanup

- [ ] Update `go/WORKFLOW.md` with a `claude_sdk` example.
- [ ] Update `go/README.md` package summary to mention the new runtime.
- [ ] Decide whether to deprecate `claude` (CLI) — recommendation: keep both, document tradeoffs.
