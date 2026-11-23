package daemon

// The process-storm guard (sysinfo/storm.go): many copies of one command line, started within
// minutes, together holding gigabytes. 2026-11-23: a git hook that called itself grew to 4,900
// sh.exe (about 44 GB) and free RAM fell to 0.1 GB while the human gamed; nothing in firefighter
// acted, because each copy was tiny and none was a job.

import (
	"fmt"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

const stormKillCount = 1000 // this many copies is stopped even with RAM to spare: it only grows

// scanStorms runs outside the lock, at most every 10 s.
func (d *Daemon) scanStorms(now time.Time, cfg config.Config, governed map[int]bool) {
	if cfg.Guard.StormProcesses <= 0 || now.Sub(d.stormAt) < 10*time.Second {
		return
	}
	d.stormAt = now
	d.storms = sysinfo.Storms(sysinfo.StormParams{MinCount: cfg.Guard.StormProcesses, MinGB: 2, Within: 10 * time.Minute}, governed)
}

// handleStorms stops a storm when the human's RAM is short (or it is past stormKillCount), and
// otherwise says it is there. Runs under the lock, in the loop.
func (d *Daemon) handleStorms(now time.Time) {
	d.stormNotes = nil
	for _, s := range d.storms {
		short := d.snap.RAMAvailGB > 0 && d.snap.RAMAvailGB < d.ramFloor()
		what := fmt.Sprintf("%d copies of %s (%.1f GB) started within minutes: %s", len(s.PIDs), s.Name, s.RAMGB, clip(s.Cmdline, 160))
		if !short && len(s.PIDs) < stormKillCount {
			d.stormNotes = append(d.stormNotes, "process storm: "+what+"; it is stopped if RAM runs under the human's reserve or it reaches 1000 copies")
			continue
		}
		killed := 0
		for i, pid := range s.PIDs {
			if sysinfo.Same(pid, s.Created[i]) && sysinfo.Kill(pid) == nil {
				killed++
			}
		}
		why := "RAM is under the human's reserve"
		if !short {
			why = fmt.Sprintf("it reached %d copies", len(s.PIDs))
		}
		logf("process storm stopped (%s): killed %d of %s", why, killed, what)
		d.stormNotes = append(d.stormNotes, fmt.Sprintf("process storm stopped at %s (%s): killed %d of %s", clock(now), why, killed, what))
	}
	d.storms = nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(sysinfo.Printable(s))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
