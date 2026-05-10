//go:build !windows

package durable

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireDirLock opens (creating if needed) the lock file at path and
// takes an exclusive non-blocking flock. The returned *os.File must be
// retained for the lifetime of the lock; closing it releases the lock.
func acquireDirLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLockHeld
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return f, nil
}

// releaseDirLock unlocks and closes f. Best-effort; errors are returned
// but the caller typically logs and continues.
func releaseDirLock(f *os.File) error {
	if f == nil {
		return nil
	}
	// Unlock is implied by close, but be explicit so callers can observe
	// any unlock-specific error.
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
