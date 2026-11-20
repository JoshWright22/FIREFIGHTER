//go:build !windows

package sysinfo

// commit: Linux and macOS overcommit, so admission goes by available RAM alone there.
func commit() (limit, used float64) { return 0, 0 }
