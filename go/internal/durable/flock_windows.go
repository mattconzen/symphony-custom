//go:build windows

package durable

import "os"

// acquireDirLock is a no-op on Windows; multi-process safety is not yet
// implemented for that platform. Returns nil so the Store still
// constructs cleanly.
func acquireDirLock(path string) (*os.File, error) {
	return nil, nil
}

// releaseDirLock is a no-op on Windows.
func releaseDirLock(f *os.File) error {
	return nil
}
