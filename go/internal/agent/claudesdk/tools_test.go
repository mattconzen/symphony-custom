package claudesdk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBashTool_RunsInWorkspace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644))

	out, err := bashTool{}.Run(context.Background(), dir, json.RawMessage(`{"command":"ls"}`))
	require.NoError(t, err)
	assert.Contains(t, out, "hello.txt")
}

func TestBashTool_Timeout(t *testing.T) {
	out, err := bashTool{}.Run(context.Background(), t.TempDir(), json.RawMessage(`{"command":"sleep 2","timeout_ms":50}`))
	require.NoError(t, err)
	assert.Contains(t, out, "timed out")
}

func TestReadTool_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644))

	out, err := readTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"a.txt"}`))
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

func TestReadTool_RejectsOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := readTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"../escape.txt"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the workspace")
}

func TestWriteTool_CreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	_, err := writeTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"a/b/c.txt","content":"x"}`))
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

func TestEditTool_UniqueReplacement(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("foo bar baz"), 0o644))

	_, err := editTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"f.txt","old_string":"bar","new_string":"qux"}`))
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	assert.Equal(t, "foo qux baz", string(data))
}

func TestEditTool_NonUniqueRejected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("aa aa aa"), 0o644))

	_, err := editTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"f.txt","old_string":"aa","new_string":"bb"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not unique")
}

func TestEditTool_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("aa aa aa"), 0o644))

	_, err := editTool{}.Run(context.Background(), dir, json.RawMessage(`{"path":"f.txt","old_string":"aa","new_string":"bb","replace_all":true}`))
	require.NoError(t, err)
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	assert.Equal(t, "bb bb bb", string(data))
}

func TestGlobTool_Matches(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.md"), []byte("text"), 0o644))

	out, err := globTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"**/*.go"}`))
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "a.go"), "expected a.go in %q", out)
	assert.False(t, strings.Contains(out, "b.md"))
}

func TestGrepTool_Matches(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nbeta\ngamma"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("delta\nbetabeta"), 0o644))

	out, err := grepTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"beta"}`))
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt:2:beta")
	assert.Contains(t, out, "b.txt:2:betabeta")
}

func TestRegistry_RoundTrip(t *testing.T) {
	r := newToolRegistry(defaultTools()...)
	assert.Equal(t, 6, len(r.defs()))
	_, ok := r.lookup("Bash")
	assert.True(t, ok)
	_, ok = r.lookup("nope")
	assert.False(t, ok)
}
