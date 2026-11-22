//go:build windows

package sysinfo

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/windows/registry"
)

// commit is the system commit limit and charge (GB), and the most the limit can grow to: Windows
// grows a system-managed (or min/max) page file on demand, so a limit that is nearly reached is no
// shortage while the page file may still grow (2026-11-22: 43 of 48 GB committed with 16 GB of
// page file allocated and 1.9 GB of it in use, and every GPU job held back for "commit").
func commit() (limit, used, most float64) {
	v, err := mem.NewExWindows().VirtualMemory()
	if err != nil {
		return 0, 0, 0
	}
	limit, used = gb(v.CommitLimit), gb(v.CommitTotal)
	total := 0.0
	if m, err := mem.VirtualMemory(); err == nil {
		total = gb(m.Total)
	}
	return limit, used, math.Max(limit, total+pagefileMax(total, limit-total))
}

var pf struct {
	sync.Mutex
	at   time.Time
	most float64
}

// pagefileMax is the largest the page files may grow to (GB), read at most once a minute: the
// configured maximum of each, or for a system-managed one 3x RAM (at least 4 GB) and at most an
// eighth of its drive, as Windows sizes it; either way no more than the drive has free beyond 10 GB.
func pagefileMax(ramGB, nowGB float64) float64 {
	pf.Lock()
	defer pf.Unlock()
	if time.Since(pf.at) < time.Minute {
		return math.Max(pf.most, nowGB)
	}
	pf.at, pf.most = time.Now(), math.Max(0, nowGB)
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management`, registry.QUERY_VALUE)
	if err != nil {
		return pf.most
	}
	defer k.Close()
	files, _, err := k.GetStringsValue("PagingFiles")
	if err != nil {
		return pf.most
	}
	sum := 0.0
	for _, f := range files {
		sum += pagefileEntryMax(f, ramGB, func(drive string) (free, size float64, ok bool) {
			u, err := disk.Usage(drive + `\`)
			if err != nil {
				return 0, 0, false
			}
			return gb(u.Free), gb(u.Total), true
		})
	}
	pf.most = math.Max(pf.most, sum)
	return pf.most
}

// pagefileEntryMax reads one PagingFiles entry: "C:\pagefile.sys 0 0" and "?:\pagefile.sys" are
// system-managed, "C:\pagefile.sys 4096 16384" has a maximum in MB.
func pagefileEntryMax(entry string, ramGB float64, drive func(string) (free, size float64, ok bool)) float64 {
	f := strings.Fields(entry)
	if len(f) == 0 || len(f[0]) < 2 {
		return 0
	}
	letter := f[0][:2]
	if letter == "?:" {
		letter = "C:"
	}
	free, size, ok := drive(letter)
	if !ok {
		return 0
	}
	most := math.Min(math.Max(3*ramGB, 4), size/8) // system-managed
	if len(f) >= 3 {
		if mb, err := strconv.ParseFloat(f[2], 64); err == nil && mb > 0 {
			most = mb / 1024
		}
	}
	return math.Max(0, math.Min(most, free-10))
}
