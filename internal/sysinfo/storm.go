package sysinfo

// Process storms: many copies of one command started within minutes (a fork bomb, a hook that
// calls itself, a retry loop that never waits). Each copy is small, so nothing else in firefighter
// sees it; together they can take all the RAM. 2026-11-23 05:45: a global git pre-push hook that
// re-ran itself grew to 4,900 sh.exe (about 44 GB working set) and free RAM fell to 0.1 GB while
// the human was in a fullscreen game.

import (
	"sort"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Storm is one group of identical processes.
type Storm struct {
	Name    string
	Cmdline string
	PIDs    []int
	Created []int64 // each pid's start time as Created reports it, for killing only the same processes later
	RAMGB   float64
}

// StormParams: what counts as a storm.
type StormParams struct {
	MinCount int           // copies of one exact command line
	MinGB    float64       // their working sets together
	Within   time.Duration // most of them started this recently
}

// procInfo is what Storms needs of one process (so the grouping is testable without the OS).
type procInfo struct {
	pid     int
	name    string
	cmdline func() string
	ramGB   func() float64
	created int64 // ms since the epoch
}

// Storms lists the process storms running now. skip: processes not to consider (jobs, firefighter).
func Storms(p StormParams, skip map[int]bool) []Storm {
	if p.MinCount <= 0 {
		return nil
	}
	ps, err := process.Processes()
	if err != nil {
		return nil
	}
	infos := make([]procInfo, 0, len(ps))
	for _, pr := range ps {
		pr := pr
		n, err := pr.Name()
		if err != nil {
			continue
		}
		created, _ := pr.CreateTime()
		infos = append(infos, procInfo{pid: int(pr.Pid), name: n, created: created,
			cmdline: func() string { c, _ := pr.Cmdline(); return c },
			ramGB: func() float64 {
				if m, err := pr.MemoryInfo(); err == nil {
					return gb(m.RSS)
				}
				return 0
			}})
	}
	storms := findStorms(infos, p, skip, time.Now())
	// Identity for the kill (Same) is Created's own clock: kernel ticks on Linux, not gopsutil's
	// milliseconds. Comparing the two never matched, so no storm process was ever killed off
	// Windows x64 (CI 11/23, bug 566fd1a7).
	for _, s := range storms {
		for i, pid := range s.PIDs {
			s.Created[i] = Created(pid)
		}
	}
	return storms
}

// findStorms groups by name first (cheap), then, only for names with enough copies, by exact
// command line; a group is a storm when it has MinCount copies, most started within Within, and
// together hold MinGB.
func findStorms(infos []procInfo, p StormParams, skip map[int]bool, now time.Time) []Storm {
	byName := map[string][]procInfo{}
	for _, in := range infos {
		if !skip[in.pid] {
			byName[in.name] = append(byName[in.name], in)
		}
	}
	var out []Storm
	for name, list := range byName {
		if len(list) < p.MinCount {
			continue
		}
		byCmd := map[string][]procInfo{}
		for _, in := range list {
			c := in.cmdline()
			byCmd[c] = append(byCmd[c], in)
		}
		for cmd, group := range byCmd {
			if len(group) < p.MinCount || cmd == "" {
				continue
			}
			recent := 0
			for _, in := range group {
				if in.created > 0 && now.Sub(time.UnixMilli(in.created)) <= p.Within {
					recent++
				}
			}
			if recent*2 < len(group) {
				continue // a long-lived pool (a server's workers), not a storm
			}
			s := Storm{Name: name, Cmdline: cmd}
			for _, in := range group {
				s.PIDs = append(s.PIDs, in.pid)
				s.Created = append(s.Created, in.created)
				s.RAMGB += in.ramGB()
			}
			if s.RAMGB >= p.MinGB {
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].RAMGB > out[b].RAMGB })
	return out
}
