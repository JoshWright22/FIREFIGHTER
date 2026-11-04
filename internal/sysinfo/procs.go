package sysinfo

import (
	"path/filepath"
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
			n = printable(n)
			out = append(out, Proc{PID: int(p.Pid), Name: strings.ToLower(n), CPU: cpu, RAMGB: ram})
		}
	}
	s.prev, s.at = next, now
	return out
}

// AgentAncestor returns the name of the nearest ancestor whose name contains one of names.
// AgentAncestorPID is the pid of the nearest agent CLI above pid (pid itself counts), 0 if none:
// it identifies which agent session a request comes from.
func AgentAncestorPID(pid int, names []string) int {
	p, err := process.NewProcess(int32(pid))
	for depth := 0; err == nil && p != nil && depth < 30; depth++ {
		n, _ := p.Name()
		n = strings.TrimSuffix(strings.ToLower(n), ".exe")
		for _, a := range names {
			if a = strings.ToLower(a); n == a || strings.HasPrefix(n, a+"-") || strings.HasPrefix(n, a+"_") {
				return int(p.Pid)
			}
		}
		child := p
		p, err = p.Parent()
		if err == nil && p != nil && youngerThan(p, child) {
			break
		}
	}
	return 0
}

// HumanSession says whether pid was started by the human: its parent chain, unbroken (no reused or
// vanished parent), reaches a human host (terminal, desktop shell) before any agent CLI or agent
// host (node, an IDE). An orphaned helper or a script under node isn't the human.
func HumanSession(pid int, humans, agents, agentHosts []string) bool {
	match := func(n string, list []string) bool {
		for _, a := range list {
			if a = strings.ToLower(a); n == a || strings.HasPrefix(n, a+"-") || strings.HasPrefix(n, a+"_") {
				return true
			}
		}
		return false
	}
	p, err := process.NewProcess(int32(pid))
	for depth := 0; err == nil && p != nil && depth < 40; depth++ {
		n, _ := p.Name()
		n = strings.TrimSuffix(strings.ToLower(n), ".exe")
		if match(n, agents) || match(n, agentHosts) { // the caller itself too (node calling directly)
			return false
		}
		if depth > 0 && match(n, humans) && systemExe(p) {
			// A real terminal (installed where agents can't write, so not a renamed copy) above
			// the caller. Above it, still no agent: a terminal an agent launched isn't the human's.
			return !agentAbove(p, agents, agentHosts, match)
		}
		child := p
		p, err = p.Parent()
		if err != nil || p == nil || p.Pid <= 1 || youngerThan(p, child) {
			return false // chain broken (orphaned, detached) or reached init: not provably the human
		}
	}
	return false
}

// systemExe says whether a process runs from where only an administrator installs programs, so its
// name can be trusted (a copy of cmd.exe renamed WindowsTerminal.exe in a user folder can't).
func systemExe(p *process.Process) bool {
	exe, err := p.Exe()
	if err != nil || exe == "" {
		// Linux hides /proc/<pid>/exe of privileged processes. Only a real sshd or login session
		// is trusted that way: the process and its root parent both carry those names (an agent
		// can rename its own process, never a root one).
		n, _ := p.Name()
		parent, perr := p.Parent()
		if perr != nil || parent == nil || parent.Pid <= 1 || !loginName(n) {
			return false
		}
		pn, _ := parent.Name()
		uids, uerr := parent.Uids()
		return uerr == nil && len(uids) > 0 && uids[0] == 0 && loginName(pn)
	}
	e := strings.ToLower(filepath.ToSlash(exe))
	trusted := false
	for _, dir := range []string{"c:/program files/", "c:/program files (x86)/", "c:/windows/system32/", "c:/windows/syswow64/",
		"/usr/", "/bin/", "/sbin/", "/opt/", "/applications/", "/system/", "/snap/"} {
		if strings.HasPrefix(e, dir) {
			trusted = true
			break
		}
	}
	// And installed by an administrator: the program and every folder above it are owned by the
	// system (root / TrustedInstaller, SYSTEM, Administrators) and not writable by others. A
	// folder an agent made in a writable system area, even made read-only, is still its own.
	return trusted && adminOwned(exe)
}

func loginName(n string) bool {
	n = strings.ToLower(n)
	return n == "sshd" || n == "sshd-session" || n == "login" || strings.HasPrefix(n, "sshd:")
}

// agentAbove reports an agent CLI or agent host anywhere above p.
func agentAbove(p *process.Process, agents, agentHosts []string, match func(string, []string) bool) bool {
	for depth := 0; p != nil && depth < 40; depth++ {
		child := p
		parent, err := p.Parent()
		if err != nil || parent == nil || parent.Pid <= 1 || youngerThan(parent, child) {
			return false
		}
		n, _ := parent.Name()
		n = strings.TrimSuffix(strings.ToLower(n), ".exe")
		if match(n, agents) || match(n, agentHosts) {
			return true
		}
		p = parent
	}
	return false
}

func AgentAncestor(pid int, names []string) string {
	p, err := process.NewProcess(int32(pid))
	for depth := 0; err == nil && depth < 20; depth++ {
		p, err = p.Parent()
		if err != nil || p == nil {
			return ""
		}
		n, _ := p.Name()
		n = printable(strings.TrimSuffix(strings.ToLower(n), ".exe"))
		for _, a := range names {
			// "claude" matches claude and claude-1.2, not claudette; "pi" never matches pip.
			if a = strings.ToLower(a); n == a || strings.HasPrefix(n, a+"-") || strings.HasPrefix(n, a+"_") {
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
		child := p
		p, err = p.Parent()
		if err != nil || p == nil || p.Pid <= 0 || youngerThan(p, child) {
			break // a "parent" started after its child is a reused pid, not the real parent
		}
		out = append(out, int(p.Pid))
	}
	return out
}

func youngerThan(parent, child *process.Process) bool {
	pc, err1 := parent.CreateTime()
	cc, err2 := child.CreateTime()
	return err1 == nil && err2 == nil && pc > cc
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
