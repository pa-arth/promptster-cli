//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func withBufferLock(bufferPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(bufferPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(bufferPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	handle := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	defer windows.UnlockFileEx(handle, 0, 1, 0, ol) //nolint:errcheck
	return fn()
}

// tryBufferLock runs fn under the same exclusive lock as withBufferLock, but
// returns (false, nil) at once instead of waiting when another process holds it.
func tryBufferLock(lockPath string, fn func() error) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	handle := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		if err == windows.ERROR_LOCK_VIOLATION {
			return false, nil
		}
		return false, fmt.Errorf("LockFileEx: %w", err)
	}
	defer windows.UnlockFileEx(handle, 0, 1, 0, ol) //nolint:errcheck
	return true, fn()
}
