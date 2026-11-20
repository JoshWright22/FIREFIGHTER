//go:build windows

package sysinfo

import "github.com/shirou/gopsutil/v4/mem"

// commit is the system commit limit and charge (GB).
func commit() (limit, used float64) {
	if v, err := mem.NewExWindows().VirtualMemory(); err == nil {
		return gb(v.CommitLimit), gb(v.CommitTotal)
	}
	return 0, 0
}
