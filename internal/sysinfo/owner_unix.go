//go:build !windows

package sysinfo

import (
	"os"
	"path/filepath"
	"syscall"
)

// adminOwned: path and every folder above it are owned by root and not writable by group or
// others (sticky world-writable folders like /tmp don't qualify).
func adminOwned(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
