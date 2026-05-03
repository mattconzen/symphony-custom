package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve_Defaults(t *testing.T) {
	wf := domain.Workflow{
		Config:         map[string]any{},
		PromptTemplate: "",
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	// Tracker defaults
	assert.Equal(t, []string{"Todo", "In Progress"}, cfg.Tracker.ActiveStates)
	assert.Equal(t, []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}, cfg.Tracker.TerminalStates)

	// Polling defaults
	assert.Equal(t, 30000, cfg.Polling.IntervalMs)

	// Workspace defaults
	assert.True(t, filepath.IsAbs(cfg.Workspace.Root))

	// Hooks defaults
	assert.Equal(t, 60000, cfg.Hooks.TimeoutMs)
	assert.Empty(t, cfg.Hooks.BetweenTurns)

	// Agent defaults
	assert.Equal(t, "codex", cfg.Agent.Runtime)
	assert.Equal(t, 10, cfg.Agent.MaxConcurrentAgents)
	assert.Equal(t, 20, cfg.Agent.MaxTurns)
	assert.Equal(t, 300000, cfg.Agent.MaxRetryBackoffMs)

	// Codex defaults
	assert.Equal(t, "codex app-server", cfg.Codex.Command)
	assert.Equal(t, 3600000, cfg.Codex.TurnTimeoutMs)
	assert.Equal(t, 5000, cfg.Codex.ReadTimeoutMs)
	assert.Equal(t, 300000, cfg.Codex.StallTimeoutMs)

	// Claude defaults
	assert.Contains(t, cfg.Claude.Command, "claude --print")
	assert.Equal(t, 3600000, cfg.Claude.TurnTimeoutMs)
	assert.Equal(t, 5000, cfg.Claude.ReadTimeoutMs)
	assert.Equal(t, 300000, cfg.Claude.StallTimeoutMs)
}

func TestResolve_ExplicitValues(t *testing.T) {
	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":            "memory",
				"active_states":   []any{"In Progress", "Review"},
				"terminal_states": []any{"Done", "Cancelled"},
			},
			"polling": map[string]any{
				"interval_ms": 5000,
			},
			"agent": map[string]any{
				"runtime":               "mock",
				"max_concurrent_agents": 5,
				"max_turns":             10,
			},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "memory", cfg.Tracker.Kind)
	assert.Equal(t, []string{"In Progress", "Review"}, cfg.Tracker.ActiveStates)
	assert.Equal(t, []string{"Done", "Cancelled"}, cfg.Tracker.TerminalStates)
	assert.Equal(t, 5000, cfg.Polling.IntervalMs)
	assert.Equal(t, "mock", cfg.Agent.Runtime)
	assert.Equal(t, 5, cfg.Agent.MaxConcurrentAgents)
	assert.Equal(t, 10, cfg.Agent.MaxTurns)
}

func TestResolve_VarExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "my-secret-key")

	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":    "linear",
				"api_key": "$TEST_API_KEY",
			},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "my-secret-key", cfg.Tracker.APIKey)
}

func TestResolve_WorkspaceRootAbsolute(t *testing.T) {
	tmpDir := t.TempDir()
	wf := domain.Workflow{
		Config: map[string]any{
			"workspace": map[string]any{
				"root": "relative/path",
			},
		},
	}
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	assert.True(t, filepath.IsAbs(cfg.Workspace.Root))
	assert.Equal(t, filepath.Join(tmpDir, "relative/path"), cfg.Workspace.Root)
}

func TestPreflight_MissingTrackerKind(t *testing.T) {
	wf := domain.Workflow{Config: map[string]any{}}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	err = config.Preflight(cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, config.ErrMissingTrackerKind))
}

func TestPreflight_InvalidAgentRuntime(t *testing.T) {
	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{"kind": "memory"},
			"agent":   map[string]any{"runtime": "invalid"},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	err = config.Preflight(cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, config.ErrInvalidAgentRuntime))
}

func TestPreflight_ValidMemoryMock(t *testing.T) {
	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{"kind": "memory"},
			"agent":   map[string]any{"runtime": "mock"},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	err = config.Preflight(cfg)
	require.NoError(t, err)
}

func TestPreflight_ValidCodexRuntime(t *testing.T) {
	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{"kind": "memory"},
			"agent":   map[string]any{"runtime": "codex"},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	err = config.Preflight(cfg)
	require.NoError(t, err)
}

func TestPreflight_BetweenTurnsRequiresTimeout(t *testing.T) {
	wf := domain.Workflow{
		Config: map[string]any{
			"tracker": map[string]any{"kind": "memory"},
			"agent":   map[string]any{"runtime": "mock"},
			"hooks": map[string]any{
				"between_turns": "echo hi",
				"timeout_ms":    0,
			},
		},
	}
	tmpDir := t.TempDir()
	cfg, err := config.Resolve(wf, tmpDir)
	require.NoError(t, err)

	err = config.Preflight(cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, config.ErrBetweenTurnsRequiresTimeout))
}

func TestWatcher_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	wfPath := filepath.Join(tmpDir, "WORKFLOW.md")

	content1 := "---\ntracker:\n  kind: memory\nagent:\n  runtime: mock\n---\nHello\n"
	require.NoError(t, os.WriteFile(wfPath, []byte(content1), 0644))

	ctx := t.Context()
	ch, stop, err := config.Watch(ctx, wfPath)
	require.NoError(t, err)
	defer stop()

	// Should receive initial config
	select {
	case cfg := <-ch:
		assert.Equal(t, "memory", cfg.Tracker.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial config")
	}

	// Update file
	content2 := "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 9999\nagent:\n  runtime: mock\n---\nHello\n"
	require.NoError(t, os.WriteFile(wfPath, []byte(content2), 0644))

	// Should receive updated config
	select {
	case cfg := <-ch:
		assert.Equal(t, 9999, cfg.Polling.IntervalMs)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for updated config")
	}
}

func TestWatcher_InvalidReloadKeepsLastGood(t *testing.T) {
	tmpDir := t.TempDir()
	wfPath := filepath.Join(tmpDir, "WORKFLOW.md")

	content1 := "---\ntracker:\n  kind: memory\nagent:\n  runtime: mock\n---\nHello\n"
	require.NoError(t, os.WriteFile(wfPath, []byte(content1), 0644))

	ctx := t.Context()
	ch, stop, err := config.Watch(ctx, wfPath)
	require.NoError(t, err)
	defer stop()

	// Initial config
	select {
	case cfg := <-ch:
		assert.Equal(t, "memory", cfg.Tracker.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial config")
	}

	// Write invalid YAML
	require.NoError(t, os.WriteFile(wfPath, []byte("---\n[invalid yaml\n---\n"), 0644))

	// Write valid config again
	content3 := "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 7777\nagent:\n  runtime: mock\n---\nHello\n"
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(wfPath, []byte(content3), 0644))

	// Should eventually receive the valid config (invalid was skipped)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case cfg := <-ch:
			if cfg.Polling.IntervalMs == 7777 {
				return // Success
			}
		case <-deadline:
			t.Fatal("timed out waiting for valid reload")
		}
	}
}
