package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// rogue is a process an agent CLI started outside firefighter that has stayed heavy.
type rogue struct {
	proc    sysinfo.Proc
	agent   string
	project string
	gpu     bool
	vramGB  float64
	created int64
	owner   int // the agent session above it
}

// findRogues runs outside the daemon lock (it scans every process). It returns processes that
// descend from an agent CLI (guard.agent_parents) and have been heavy (CPU, RAM, or GPU compute)
// for guard.heavy_seconds, and aren't already governed.
func (d *Daemon) findRogues(now time.Time, c config.Config, snap sysinfo.Snapshot, governed map[int]bool) []rogue {
	g := c.Guard
	if !g.AutoAdopt || now.Sub(d.rogueAt) < 5*time.Second {
		return nil
	}
	d.rogueAt = now
	if d.scanner == nil {
		d.scanner = sysinfo.NewScanner()
	}
	heavy := map[int]sysinfo.Proc{}
	for _, p := range d.scanner.Scan(g.HeavyCPUs, g.HeavyRAMGB) {
		heavy[p.PID] = p
	}
	for pid, u := range snap.ProcGPU {
		if u.Util-u.Util3D >= c.Reserve.ForeignGPUPct { // compute, not graphics
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
	for pid, a := range d.agentOf {
		if !sysinfo.Same(pid, a.created) {
			delete(d.agentOf, pid) // gone
		}
	}
	var out []rogue
	for pid, p := range heavy {
		if governed[pid] || pid <= 4 {
			continue
		}
		if fg := strings.ToLower(snap.ForegroundApp); fg != "" &&
			strings.TrimSuffix(strings.ToLower(p.Name), ".exe") == strings.TrimSuffix(fg, ".exe") {
			continue // the app the human is using (a game or tool an agent opened for them) is theirs
		}
		if u := snap.ProcGPU[pid]; u.Util3D >= 5 {
			continue // drawing on screen: something the human looks at, not background compute
		}
		if d.agentOf == nil {
			d.agentOf = map[int]agentSeen{}
		}
		created := sysinfo.Created(pid)
		if _, ok := d.agentOf[pid]; !ok {
			// Remember who started it while its parents are still there: a `nohup ... &` whose
			// shell exits is still the agent's work.
			if a := sysinfo.AgentAncestor(pid, g.AgentParents); a != "" {
				d.agentOf[pid] = agentSeen{name: a, created: created, owner: sysinfo.AgentAncestorPID(pid, g.AgentParents)}
			}
		}
		first, ok := d.rogueSeen[pid]
		if !ok {
			d.rogueSeen[pid] = now
			continue
		}
		if now.Sub(first).Seconds() < g.HeavySeconds {
			continue
		}
		agent, owner := sysinfo.AgentAncestor(pid, g.AgentParents), sysinfo.AgentAncestorPID(pid, g.AgentParents)
		if seen, ok := d.agentOf[pid]; agent == "" && ok && seen.created == created {
			agent, owner = seen.name, seen.owner
		}
		if agent == "" {
			d.rogueSeen[pid] = now.Add(time.Hour) // the human's own program: look again in an hour
			continue
		}
		delete(d.rogueSeen, pid)
		project := "unmanaged"
		if cwd := sysinfo.Cwd(pid); cwd != "" {
			project = cleanText(projectOf(cwd), 100)
		}
		u := snap.ProcGPU[pid]
		out = append(out, rogue{proc: p, agent: agent, project: project, gpu: u.Util-u.Util3D >= c.Reserve.ForeignGPUPct,
			vramGB: u.VRAMGB, created: created, owner: owner})
	}
	return out
}

// adoptRogues turns found rogues into spot-band jobs: idle priority, the human's CPU reserve, a
// charge to their project at 1.5x, and first in line for preemption (checkpoint file, then
// SIGTERM, then kill), so skipping the queue never pays off.
func (d *Daemon) adoptRogues(rs []rogue, now time.Time) {
	for _, r := range rs {
		if r.created == 0 || !sysinfo.Same(r.proc.PID, r.created) {
			continue
		}
		dup := false
		for _, j := range d.jobs {
			if _, ok := j.pids[r.proc.PID]; ok && j.State != "done" {
				dup = true
			}
		}
		if dup {
			continue
		}
		t := &policy.Ticket{ID: NewID(), Project: r.project, Name: fmt.Sprintf("unmanaged %s (started by %s)", r.proc.Name, r.agent),
			GPU: r.gpu, Minutes: 30, Bid: 5, Evict: "graceful", Band: policy.BandSpot, State: "running",
			Created: now, Started: now, PID: r.proc.PID, ClientPID: r.proc.PID, PredMinutes: 30, NeedRAM: r.proc.RAMGB, NeedVRAM: r.vramGB}
		j := newJob(t)
		j.adopted = true
		j.ownerAgent, j.ownerCreated = r.owner, sysinfo.Created(r.owner)
		_ = sysinfo.Background(r.proc.PID)
		d.lockPrice(j, now)
		d.jobs[t.ID] = j
		logf("%s: adopted pid %d (%s, heavy outside firefighter for %.0fs), spot band", t.Name, r.proc.PID, r.project, d.cfg.Guard.HeavySeconds)
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

// agentSeen: an agent found above a heavy process while its parents were still there.
type agentSeen struct {
	name    string
	created int64
	owner   int
}
