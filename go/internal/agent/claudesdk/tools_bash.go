package claudesdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// bashTool runs a shell command inside the workspace directory via `bash -lc`.
// There is no allow-list — the runtime relies on the workspace being a
// sandboxed per-issue scratch directory (SPEC §9). Operators with stricter
// requirements should use the CLI `claude` runtime, which carries its own
// permission UX.
type bashTool struct{}

func (bashTool) Name() string { return "Bash" }

func (bashTool) Description() string {
	return "Run a shell command via bash -lc inside the workspace. Returns combined stdout+stderr. " +
		"Use this for git, npm, pytest, etc. Long-running commands MUST set a tight 'timeout_ms'."
}

func (bashTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to run.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"description": "Wall-clock timeout in milliseconds. Default 120000 (2 min).",
			},
		},
		"required": []string{"command"},
	}
}

type bashInput struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}

func (bashTool) Run(ctx context.Context, workspacePath string, input json.RawMessage) (string, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bash: invalid input: %w", err)
	}
	if in.Command == "" {
		return "", fmt.Errorf("bash: empty command")
	}
	if in.TimeoutMs <= 0 {
		in.TimeoutMs = 120_000
	}

	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "bash", "-lc", in.Command) //nolint:gosec
	cmd.Dir = workspacePath
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()
	// Clip output to keep tool_result blocks reasonable; Symphony's per-issue
	// transcript captures the full byte stream separately.
	if len(out) > 64*1024 {
		out = out[:64*1024] + "\n…[truncated]\n"
	}

	if cctx.Err() == context.DeadlineExceeded {
		return out + fmt.Sprintf("\n[bash: timed out after %dms]", in.TimeoutMs), nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out + fmt.Sprintf("\n[bash: exit %d]", ee.ExitCode()), nil
		}
		return out + fmt.Sprintf("\n[bash: %v]", err), nil
	}
	return out, nil
}
