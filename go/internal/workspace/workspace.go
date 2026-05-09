// Package workspace manages per-issue workspace directories and lifecycle hooks
// per SPEC §9.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

// HookResult captures the outcome of running a workspace hook.
type HookResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	TimedOut bool
}

// Manager manages workspace lifecycle for issues per SPEC §9.
type Manager struct {
	cfg config.Config
}

// NewManager returns a new Manager with the given configuration.
func NewManager(cfg config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// EnsureForIssue creates the workspace directory for the issue if it does not
// exist, and returns a Workspace. CreatedNow is true only when the directory
// was created during this call per SPEC §9.2.
func (m *Manager) EnsureForIssue(ctx context.Context, issue domain.Issue) (domain.Workspace, error) {
	key := issue.WorkspaceKey()
	path := filepath.Join(m.cfg.Workspace.Root, key)
	path = filepath.Clean(path)

	// Safety check
	if err := m.validatePath(path); err != nil {
		return domain.Workspace{}, err
	}

	// Check if it already exists
	_, statErr := os.Stat(path)
	createdNow := false

	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(path, 0755); err != nil {
			return domain.Workspace{}, fmt.Errorf("creating workspace %s: %w", path, err)
		}
		createdNow = true
	} else if statErr != nil {
		return domain.Workspace{}, fmt.Errorf("checking workspace %s: %w", path, statErr)
	}

	ws := domain.Workspace{
		Path:       path,
		Key:        key,
		CreatedNow: createdNow,
	}

	// Run after_create hook if workspace was just created
	if createdNow && m.cfg.Hooks.AfterCreate != "" {
		timeout := time.Duration(m.cfg.Hooks.TimeoutMs) * time.Millisecond
		result, err := m.RunHook(ctx, ws, m.cfg.Hooks.AfterCreate, timeout)
		if err != nil {
			return domain.Workspace{}, fmt.Errorf("after_create hook: %w", err)
		}
		if result.ExitCode != 0 || result.TimedOut {
			return domain.Workspace{}, fmt.Errorf("after_create hook failed (exit %d, timedOut=%v)",
				result.ExitCode, result.TimedOut)
		}
	}

	return ws, nil
}

// RemoveForIssue removes the workspace for the given issue if it exists per
// SPEC §9.1.
func (m *Manager) RemoveForIssue(ctx context.Context, issue domain.Issue) error {
	key := issue.WorkspaceKey()
	path := filepath.Join(m.cfg.Workspace.Root, key)
	path = filepath.Clean(path)

	ws := domain.Workspace{Path: path, Key: key}
	return m.RemoveWorkspace(ctx, ws)
}

// RemoveWorkspace removes the given workspace directory. It validates the path
// is inside the workspace root before proceeding per SPEC §9.5.
func (m *Manager) RemoveWorkspace(ctx context.Context, ws domain.Workspace) error {
	if err := m.validatePath(ws.Path); err != nil {
		return err
	}

	// Check if it exists
	_, err := os.Stat(ws.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat workspace %s: %w", ws.Path, err)
	}

	// Run before_remove hook
	if m.cfg.Hooks.BeforeRemove != "" {
		timeout := time.Duration(m.cfg.Hooks.TimeoutMs) * time.Millisecond
		_, _ = m.RunHook(ctx, ws, m.cfg.Hooks.BeforeRemove, timeout) // failure logged, ignored
	}

	return os.RemoveAll(ws.Path)
}

// RunHook executes a shell script in the workspace directory via `bash -lc`
// per SPEC §9.4. It captures stdout+stderr, respects the given timeout, and
// kills the process on context deadline or timeout.
func (m *Manager) RunHook(ctx context.Context, ws domain.Workspace, script string, timeout time.Duration) (HookResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	cmd.Dir = ws.Path

	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()

	result := HookResult{
		Stdout: []byte(stdoutBuf.String()),
		Stderr: []byte(stderrBuf.String()),
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		return result, nil
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}

	return result, nil
}

// validatePath checks that the workspace path is inside the workspace root per
// SPEC §9.5 Invariant 2.
func (m *Manager) validatePath(path string) error {
	root := filepath.Clean(m.cfg.Workspace.Root)
	absPath := filepath.Clean(path)

	// Require absPath to have root as a prefix directory component.
	if absPath == root {
		return fmt.Errorf("workspace path %q equals root, outside workspace root", absPath)
	}

	if !strings.HasPrefix(absPath, root+string(filepath.Separator)) {
		return fmt.Errorf("workspace path %q is outside workspace root %q", absPath, root)
	}

	return nil
}
