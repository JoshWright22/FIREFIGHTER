//go:build !windows

package sysinfo

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// CanTrim: only Windows can move a suspended process's memory out of RAM from outside (see Trim).
// Elsewhere a stopped process keeps its RAM unless the kernel happens to swap it.
const CanTrim = false

func Trim(int) error { return errors.ErrUnsupported }

// Linux and macOS count their own file cache as available memory already, and there is no local
// VM whose cache hides from the host the way WSL2's does.
func vmHost() float64                       { return 0 }
func vmGuest() (name, info string, ok bool) { return "", "", false }
func vmReclaim(string) error                { return errors.ErrUnsupported }

// volume is the mount point a path is on: the highest folder still on the same device.
func volume(abs string) string {
	dev := func(p string) (uint64, bool) {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return uint64(st.Dev), true // Dev is int32 on macOS
	}
	p := abs
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		if filepath.Dir(p) == p {
			return p
		}
		p = filepath.Dir(p) // a folder that doesn't exist yet: its nearest real parent
	}
	want, ok := dev(p)
	for ok {
		up := filepath.Dir(p)
		if d, ok2 := dev(up); up == p || !ok2 || d != want {
			break
		}
		p = up
	}
	return p
}
