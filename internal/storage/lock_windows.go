//go:build windows

package storage

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive OS lock on f, or fails at once if another process holds it.
func tryLock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
}
