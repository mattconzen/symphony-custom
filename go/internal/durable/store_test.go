package durable_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/durable"
)

type payload struct {
	Hello string `json:"hello"`
	N     int    `json:"n"`
}

func TestStore_RoundTrip(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)

	want := payload{Hello: "world", N: 42}
	require.NoError(t, s.Save("orchestrator", want))

	var got payload
	require.NoError(t, s.Load("orchestrator", &got))
	assert.Equal(t, want, got)
}

func TestStore_RoundTripFsyncsParentDir(t *testing.T) {
	// Round-trip with the fsync-parent-dir path active. We can't directly
	// observe the dir fsync from user space, but we assert the new code
	// path doesn't break the happy path.
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Save("orchestrator", payload{Hello: "fsync", N: 7}))
	var got payload
	require.NoError(t, s.Load("orchestrator", &got))
	assert.Equal(t, "fsync", got.Hello)
}

func TestStore_LoadMissing(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)
	var p payload
	err = s.Load("nope", &p)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestStore_AtomicRenameNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	s, err := durable.New(dir)
	require.NoError(t, err)

	require.NoError(t, s.Save("orchestrator", payload{Hello: "first", N: 1}))
	require.NoError(t, s.Save("orchestrator", payload{Hello: "second", N: 2}))

	var got payload
	require.NoError(t, s.Load("orchestrator", &got))
	assert.Equal(t, "second", got.Hello)

	// No leftover tmp files.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp.", "leftover tmp file: %s", e.Name())
	}
}

func TestStore_SchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	// Write a v999 envelope by hand.
	path := filepath.Join(dir, "x.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":999,"data":{}}`), 0o644))

	s, err := durable.New(dir)
	require.NoError(t, err)
	var p payload
	err = s.Load("x", &p)
	require.Error(t, err)
	assert.True(t, errors.Is(err, durable.ErrSchemaMismatch))
}

func TestStore_ConcurrentSavesSerialiseSameName(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			assert.NoError(t, s.Save("hot", payload{N: n}))
		}(i)
	}
	wg.Wait()

	var got payload
	require.NoError(t, s.Load("hot", &got))
}

func TestStore_NestedNameCreatesDir(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Save("trackers/memory_issues", payload{Hello: "ok"}))
	var got payload
	require.NoError(t, s.Load("trackers/memory_issues", &got))
	assert.Equal(t, "ok", got.Hello)
}

func TestStore_DirLockContention(t *testing.T) {
	dir := t.TempDir()
	a, err := durable.New(dir)
	require.NoError(t, err)
	defer a.Close()

	// A second store on the same path must fail with a clear error.
	b, err := durable.New(dir)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "another symphony instance")
}

func TestStore_DirLockReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	a, err := durable.New(dir)
	require.NoError(t, err)
	require.NoError(t, a.Close())

	// After Close, a new store should be able to acquire the lock.
	b, err := durable.New(dir)
	require.NoError(t, err)
	defer b.Close()
}

func TestStore_Delete(t *testing.T) {
	s, err := durable.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.Save("ephemeral", payload{N: 1}))
	require.NoError(t, s.Delete("ephemeral"))
	var p payload
	err = s.Load("ephemeral", &p)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	// Re-deleting is fine.
	require.NoError(t, s.Delete("ephemeral"))
}
