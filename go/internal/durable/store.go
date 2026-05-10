// Package durable implements the JSON-on-disk persistence layer per SPEC
// §15.5. State is written atomically (tmp + rename) and wrapped in a
// `{version, data}` envelope so future schema bumps can fail fast.
package durable

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// syscallErrNotSupported / syscallErrInvalid surface common "directory
// sync not supported" errors as comparable sentinels. Kept as helpers so
// the call site stays readable.
func syscallErrNotSupported() error { return syscall.ENOTSUP }
func syscallErrInvalid() error      { return syscall.EINVAL }

// SchemaVersion is the current envelope version. Bump on any incompatible
// payload change. Loaders refuse to read files with a version they don't
// recognise.
const SchemaVersion = 1

// ErrSchemaMismatch is returned by Load when the stored envelope's version
// does not match SchemaVersion.
var ErrSchemaMismatch = errors.New("durable: schema version mismatch")

// errLockHeld signals that another process already holds the directory
// lock. Surfaced wrapped with a human-readable message by New().
var errLockHeld = errors.New("lock held")

// Store persists named JSON payloads under a fixed directory. The directory
// is created on construction if missing. Each named file gets its own
// mutex so concurrent saves to *different* names don't serialise.
type Store struct {
	path string

	// lockFile is the OS-level exclusive lock held for the Store's
	// lifetime to prevent two symphony processes from racing on the same
	// directory. Nil on platforms where flock is a no-op (Windows).
	lockFile *os.File

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// envelope wraps every saved payload with metadata. The Data field is
// declared as json.RawMessage so Load can return it untouched (the caller
// performs the second unmarshal into the concrete payload type).
type envelope struct {
	Version   int             `json:"version"`
	WrittenAt time.Time       `json:"written_at"`
	Data      json.RawMessage `json:"data"`
}

// New constructs a Store rooted at path. The directory is created if it
// does not yet exist; an error is returned only when the directory cannot
// be created or written to.
func New(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("durable: path is required")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("durable: mkdir %s: %w", path, err)
	}
	// Probe writability with a sentinel file.
	probe := filepath.Join(path, ".symphony.write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil { //nolint:gosec
		return nil, fmt.Errorf("durable: %s is not writable: %w", path, err)
	}
	_ = os.Remove(probe) //nolint:errcheck

	// Acquire an OS-level exclusive lock so two symphony processes
	// cannot share the same durable directory without noticing.
	lockPath := filepath.Join(path, ".symphony.lock")
	lockFile, err := acquireDirLock(lockPath)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("durable: another symphony instance already owns %s", path)
		}
		return nil, fmt.Errorf("durable: lock %s: %w", lockPath, err)
	}

	return &Store{path: path, locks: make(map[string]*sync.Mutex), lockFile: lockFile}, nil
}

// Close releases the directory lock and any associated OS resources. Safe
// to call multiple times; subsequent calls are no-ops.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if s.lockFile == nil {
		return nil
	}
	err := releaseDirLock(s.lockFile)
	s.lockFile = nil
	return err
}

// Path returns the configured store directory.
func (s *Store) Path() string { return s.path }

// lockFor returns the per-name mutex, allocating one lazily.
func (s *Store) lockFor(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[name]
	if !ok {
		m = &sync.Mutex{}
		s.locks[name] = m
	}
	return m
}

// fileFor expands a logical name to an absolute path. Subdirectories
// inside the name (e.g. "trackers/memory_issues") are honoured.
func (s *Store) fileFor(name string) string {
	return filepath.Join(s.path, name+".json")
}

// Save marshals payload to JSON inside the schema envelope and writes it
// atomically (tmp + fsync + rename) under <path>/<name>.json. Parent
// directories implied by name are created on the fly.
func (s *Store) Save(name string, payload any) error {
	if name == "" {
		return errors.New("durable: name is required")
	}
	body, err := json.Marshal(envelope{
		Version:   SchemaVersion,
		WrittenAt: time.Now().UTC(),
		Data:      json.RawMessage(mustMarshal(payload)),
	})
	if err != nil {
		return fmt.Errorf("durable: marshal envelope: %w", err)
	}

	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()

	target := s.fileFor(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("durable: mkdir %s: %w", filepath.Dir(target), err)
	}

	suffix := randomSuffix()
	tmp := target + ".tmp." + suffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("durable: open tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close() //nolint:errcheck
		_ = os.Remove(tmp)
		return fmt.Errorf("durable: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck
		_ = os.Remove(tmp)
		return fmt.Errorf("durable: fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("durable: close tmp: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("durable: rename %s -> %s: %w", tmp, target, err)
	}
	// Fsync the parent directory so the rename itself is durable: without
	// this, ext4 can lose the rename even though the file body is fsync'd.
	// Best-effort: some platforms/filesystems return ENOTSUP/EINVAL on
	// directory sync — we treat those as success.
	if err := fsyncDir(filepath.Dir(target)); err != nil {
		return fmt.Errorf("durable: fsync dir %s: %w", filepath.Dir(target), err)
	}
	return nil
}

// fsyncDir opens dir, calls Sync, and closes it. Errors that indicate the
// underlying filesystem does not support directory sync (ENOTSUP, EINVAL on
// some platforms) are swallowed so the operation is still considered
// successful.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		if errors.Is(syncErr, syscallErrNotSupported()) {
			return nil
		}
		// EINVAL on some filesystems (e.g. some FUSE mounts) means the
		// directory cannot be synced; treat as best-effort.
		if errors.Is(syncErr, syscallErrInvalid()) {
			return nil
		}
		return syncErr
	}
	return closeErr
}

// Load reads <path>/<name>.json, validates the schema version, and
// decodes the inner payload into `into`. Returns os.ErrNotExist (wrapped)
// when the file is missing so callers can fall back to seeds cleanly.
func (s *Store) Load(name string, into any) error {
	if name == "" {
		return errors.New("durable: name is required")
	}
	data, err := os.ReadFile(s.fileFor(name))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("durable: decode envelope %s: %w", name, err)
	}
	if env.Version != SchemaVersion {
		return fmt.Errorf("%w: got %d, want %d (file %s)", ErrSchemaMismatch, env.Version, SchemaVersion, name)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		return fmt.Errorf("durable: decode payload %s: %w", name, err)
	}
	return nil
}

// Delete removes the named file. Missing files are not an error.
func (s *Store) Delete(name string) error {
	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()
	err := os.Remove(s.fileFor(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Marshal errors here mean the caller passed a value the
		// stdlib JSON encoder cannot represent (channels, functions).
		// That's a programming error, not a runtime condition; panic
		// rather than silently writing garbage.
		panic(fmt.Errorf("durable: marshal payload: %w", err))
	}
	return b
}

func randomSuffix() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
