package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// adoptRogues is the fallback for agents that skip firefighter, by accident or on purpose: a
// process descended from an agent CLI that stays heavy (CPU, RAM or GPU compute) for
// guard.heavy_seconds is adopted as a spot-band job. It gets background priority, the human's CPU
// reserve, a charge to its project, and is first in line for preemption.
func (d *Daemon) adoptRogues(now time.Time) {
	g := d.cfg.Guard
	if !g.AutoAdopt || now.Sub(d.rogueAt) < 5*time.Second {
		return
	}
	d.rogueAt = now
	if d.scanner == nil {
		d.scanner = sysinfo.NewScanner()
	}
	governed := map[int]bool{os.Getpid(): true}
	for _, j := range d.jobs {
		for p := range j.pids {
			governed[p] = true
		}
		governed[j.ClientPID] = true
	}
	heavy := map[int]sysinfo.Proc{}
	for _, p := range d.scanner.Scan(g.HeavyCPUs, g.HeavyRAMGB) {
		heavy[p.PID] = p
	}
	for pid, u := range d.snap.ProcGPU {
		if u.Util-u.Util3D >= d.cfg.Reserve.ForeignGPUPct { // compute, not graphics
			if _, ok := heavy[pid]; !ok {
				heavy[pid] = sysinfo.Proc{PID: pid, Name: sysinfo.Name(pid)}
			}
		}
	}
	if d.rogueSeen == nil {
		d.rogueSeen = map[int]time.Time{}
	}
	for pid := range d.rogueSeen {
		if _, ok := heavy[pid]; !ok {
			delete(d.rogueSeen, pid)
		}
	}
	for pid, p := range heavy {
		if governed[pid] {
			continue
		}
		first, ok := d.rogueSeen[pid]
		if !ok {
			d.rogueSeen[pid] = now
			continue
		}
		if now.Sub(first).Seconds() < g.HeavySeconds {
			continue
		}
		agent := sysinfo.AgentAncestor(pid, g.AgentParents)
		if agent == "" {
			d.rogueSeen[pid] = now.Add(time.Hour) // the human's own program: look again in an hour
			continue
		}
		delete(d.rogueSeen, pid)
		project := "unmanaged"
		if cwd := sysinfo.Cwd(pid); cwd != "" {
			project = projectOf(cwd)
		}
		gpu := d.snap.ProcGPU[pid].Util-d.snap.ProcGPU[pid].Util3D >= d.cfg.Reserve.ForeignGPUPct
		t := &policy.Ticket{ID: NewID(), Project: project, Name: fmt.Sprintf("unmanaged %s (started by %s)", p.Name, agent),
			GPU: gpu, Minutes: 30, Bid: 5, Evict: "graceful", Band: policy.BandSpot, State: "running",
			Created: now, Started: now, PID: pid, ClientPID: pid, PredMinutes: 30, NeedRAM: p.RAMGB, NeedVRAM: d.snap.ProcGPU[pid].VRAMGB}
		j := &job{Ticket: t, pids: map[int]bool{pid: true}, applied: map[int]string{}, adopted: true}
		_ = sysinfo.Background(pid)
		d.lockPrice(j, now)
		d.jobs[t.ID] = j
		logf("%s: adopted pid %d (%s heavy outside firefighter for %.0fs), spot band", t.Name, pid, project, g.HeavySeconds)
		d.broadcast()
	}
}

// projectOf names the git repo (or folder) a path is in.
func projectOf(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return filepath.Base(d)
		}
		if filepath.Dir(d) == d {
			return filepath.Base(dir)
		}
	}
}
