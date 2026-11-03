package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JoshWright22/firefighter/internal/config"
)

// Lock makes sure only one guard runs per user: an OS advisory lock on daemon.lock, held for the
// daemon's whole life and released by the kernel when it dies (no stale pid files, no races).
func Lock() (func(), error) {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(config.Home(), "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("the guard is already running (%s is locked)", path)
	}
	_ = f.Truncate(0)
	fmt.Fprint(f, os.Getpid())
	return func() { f.Close() }, nil
}
