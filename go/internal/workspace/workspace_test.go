package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newManager(t *testing.T) (*workspace.Manager, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{TimeoutMs: 5000},
	}
	return workspace.NewManager(cfg), root
}

func TestEnsureForIssue_CreateOnce(t *testing.T) {
	mgr, root := newManager(t)
	issue := domain.Issue{Identifier: "ABC-123"}

	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)

	assert.Equal(t, "ABC-123", ws.Key)
	assert.Equal(t, filepath.Join(root, "ABC-123"), ws.Path)
	assert.True(t, ws.CreatedNow)

	// Directory must exist
	info, err := os.Stat(ws.Path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEnsureForIssue_Reuse(t *testing.T) {
	mgr, _ := newManager(t)
	issue := domain.Issue{Identifier: "ABC-123"}

	ws1, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)
	assert.True(t, ws1.CreatedNow)

	ws2, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)
	assert.False(t, ws2.CreatedNow, "second call should not set CreatedNow")
	assert.Equal(t, ws1.Path, ws2.Path)
}

func TestEnsureForIssue_Sanitization(t *testing.T) {
	mgr, root := newManager(t)
	issue := domain.Issue{Identifier: "ABC 123"} // space should become _

	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)
	assert.Equal(t, "ABC_123", ws.Key)
	assert.Equal(t, filepath.Join(root, "ABC_123"), ws.Path)
}

func TestRemoveForIssue(t *testing.T) {
	mgr, _ := newManager(t)
	issue := domain.Issue{Identifier: "DEL-1"}

	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)

	err = mgr.RemoveForIssue(context.Background(), issue)
	require.NoError(t, err)

	_, err = os.Stat(ws.Path)
	assert.True(t, os.IsNotExist(err))
}

func TestRunHook_Success(t *testing.T) {
	mgr, _ := newManager(t)
	issue := domain.Issue{Identifier: "HOOK-1"}
	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)

	result, err := mgr.RunHook(context.Background(), ws, "echo hello", 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, string(result.Stdout), "hello")
	assert.False(t, result.TimedOut)
}

func TestRunHook_Timeout(t *testing.T) {
	mgr, _ := newManager(t)
	issue := domain.Issue{Identifier: "HOOK-TIMEOUT"}
	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)

	result, err := mgr.RunHook(context.Background(), ws, "sleep 10", 100*time.Millisecond)
	require.NoError(t, err) // RunHook does not return error on timeout

	assert.True(t, result.TimedOut)
	assert.NotEqual(t, 0, result.ExitCode)
}

func TestRunHook_NonZeroExit(t *testing.T) {
	mgr, _ := newManager(t)
	issue := domain.Issue{Identifier: "HOOK-FAIL"}
	ws, err := mgr.EnsureForIssue(context.Background(), issue)
	require.NoError(t, err)

	result, err := mgr.RunHook(context.Background(), ws, "exit 2", 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, 2, result.ExitCode)
	assert.False(t, result.TimedOut)
}

func TestSafetyGuard_RejectOutsideRoot(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{TimeoutMs: 5000},
	}
	mgr := workspace.NewManager(cfg)

	// Craft a workspace that points outside the root
	outsideWs := domain.Workspace{
		Path: "/tmp",
		Key:  "tmp",
	}

	err := mgr.RemoveForIssue(context.Background(), domain.Issue{Identifier: "test"})
	// No error because path doesn't exist - check safety on RemoveWorkspace directly
	_ = err

	// Use the manager's safety check via a direct call
	err = mgr.RemoveWorkspace(context.Background(), outsideWs)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "outside workspace root"))
}

func TestSafetyGuard_ValidPath(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		Workspace: config.Workspace{Root: root},
		Hooks:     config.Hooks{TimeoutMs: 5000},
	}
	mgr := workspace.NewManager(cfg)

	validWs := domain.Workspace{
		Path: filepath.Join(root, "valid-key"),
		Key:  "valid-key",
	}
	// Create the directory first
	require.NoError(t, os.MkdirAll(validWs.Path, 0755))

	// Should not error - path is within root
	err := mgr.RemoveWorkspace(context.Background(), validWs)
	require.NoError(t, err)
}
