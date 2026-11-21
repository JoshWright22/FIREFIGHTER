//go:build !windows

package config

import (
	"os/user"
	"path/filepath"
	"runtime"
)

// osDefaultHomes is the real state folder from the account's home in the user database, whatever
// HOME or XDG_STATE_HOME say.
func osDefaultHomes() []string {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return nil
	}
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join(u.HomeDir, "Library", "Application Support", "firefighter")}
	}
	return []string{filepath.Join(u.HomeDir, ".local", "state", "firefighter")}
}
