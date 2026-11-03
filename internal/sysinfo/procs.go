package sysinfo

import (
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Proc is one process's recent use, for spotting heavy work started outside firefighter.
type Proc struct {
	PID   int
	Name  string
	CPU   float64 // CPUs in use since the last scan
	RAMGB float64
}

// Scanner measures every process's CPU between scans.
type Scanner struct {
	prev map[int32]float64
	at   time.Time
}

func NewScanner() *Scanner { return &Scanner{prev: map[int32]float64{}} }

// Scan returns processes using at least minCPU CPUs or minRAM GB.
func (s *Scanner) Scan(minCPU, minRAM float64) []Proc {
	ps, err := process.Processes()
	if err != nil {
		return nil
	}
	now := time.Now()
	dt := now.Sub(s.at).Seconds()
	next := make(map[int32]float64, len(ps))
	var out []Proc
	for _, p := range ps {
		t, err := p.Times()
		if err != nil {
			continue
		}
		total := t.User + t.System
		next[p.Pid] = total
		cpu := 0.0
		if old, ok := s.prev[p.Pid]; ok && dt > 0 {
			cpu = (total - old) / dt
		}
		ram := 0.0
		if m, err := p.MemoryInfo(); err == nil {
			ram = gb(m.RSS)
		}
		if cpu >= minCPU || ram >= minRAM {
			n, _ := p.Name()
			out = append(out, Proc{PID: int(p.Pid), Name: strings.ToLower(n), CPU: cpu, RAMGB: ram})
		}
	}
	s.prev, s.at = next, now
	return out
}

// AgentAncestor returns the name of the nearest ancestor whose name contains one of names.
func AgentAncestor(pid int, names []string) string {
	p, err := process.NewProcess(int32(pid))
	for depth := 0; err == nil && depth < 20; depth++ {
		p, err = p.Parent()
		if err != nil || p == nil {
			return ""
		}
		n, _ := p.Name()
		n = strings.ToLower(n)
		for _, a := range names {
			if strings.HasPrefix(n, strings.ToLower(a)) {
				return n
			}
		}
	}
	return ""
}

// Ancestors lists pid's parents, nearest first (up to 30).
func Ancestors(pid int) []int {
	var out []int
	p, err := process.NewProcess(int32(pid))
	for depth := 0; err == nil && depth < 30; depth++ {
		p, err = p.Parent()
		if err != nil || p == nil || p.Pid <= 0 {
			break
		}
		out = append(out, int(p.Pid))
	}
	return out
}

// DescendantOf says whether pid is a child (at any depth) of ancestor.
func DescendantOf(pid, ancestor int) bool {
	for _, a := range Ancestors(pid) {
		if a == ancestor {
			return true
		}
	}
	return false
}

// Cwd of a process, "" if unknown.
func Cwd(pid int) string {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return ""
	}
	c, _ := p.Cwd()
	return c
}
