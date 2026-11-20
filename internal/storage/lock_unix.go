//go:build !windows

package storage

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive OS lock on f, or fails at once if another process holds it.
func tryLock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
