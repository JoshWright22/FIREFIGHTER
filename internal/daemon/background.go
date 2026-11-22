package daemon

// Background projects: bots (an application loop, a scraper, a sync) whose work waits behind
// project work without anyone setting a bid (Josh 11/22: hub-apply's CPU jobs at bid 5 went ahead
// of three bid-7 nanoTSFM GPU jobs and held the RAM they needed).

import (
	"fmt"
	"time"
)

// ProjectSeen is how a project has submitted work lately.
type ProjectSeen struct {
	Unattended []time.Time `json:"unattended,omitempty"` // submits with neither an agent nor the human above them (scheduled tasks, services)
	Attended   []time.Time `json:"attended,omitempty"`   // submits from an agent session or the human's terminal
	GPUAt      time.Time   `json:"gpu_at,omitempty"`     // its last GPU job
}

const (
	bgWindow  = 24 * time.Hour
	bgMinRuns = 6 // unattended submits in the window
	bgHours   = 3 // spread over at least this many hours: a loop, not one burst
	bgGPUDays = 7 // a project with a GPU job this recent is research, not a bot
)

func keepRecent(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < bgWindow {
			out = append(out, t)
		}
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}

// noteSubmit records one submit: attended (an agent or the human asked) or not, and GPU or not.
func (d *Daemon) noteSubmit(project string, attended, gpu bool, now time.Time) {
	if d.st.Projects == nil {
		d.st.Projects = map[string]*ProjectSeen{}
	}
	p := d.st.Projects[project]
	if p == nil {
		p = &ProjectSeen{}
		d.st.Projects[project] = p
	}
	if attended {
		p.Attended = append(keepRecent(p.Attended, now), now)
	} else {
		p.Unattended = append(keepRecent(p.Unattended, now), now)
	}
	if gpu {
		p.GPUAt = now
	}
}

// background says whether a project's jobs wait behind project work, and why.
func (d *Daemon) background(project string, now time.Time) (bool, string) {
	if bg, fg := d.cfg.Listed(project); fg {
		return false, ""
	} else if bg {
		return true, "order.background"
	}
	p := d.st.Projects[project]
	if p == nil || (!p.GPUAt.IsZero() && now.Sub(p.GPUAt) < bgGPUDays*24*time.Hour) {
		return false, ""
	}
	un, at := keepRecent(p.Unattended, now), keepRecent(p.Attended, now)
	hours := map[int64]bool{}
	for _, t := range un {
		hours[t.Unix()/3600] = true
	}
	if len(un) < bgMinRuns || len(hours) < bgHours || len(un) < 4*len(at) {
		return false, ""
	}
	return true, fmt.Sprintf("learned: %d unattended CPU jobs over %d hours in the last day, no GPU work", len(un), len(hours))
}

// refreshBackground re-reads each open job's class (a config change, a project that became or
// stopped being a loop).
func (d *Daemon) refreshBackground(now time.Time) {
	seen := map[string]bool{}
	for _, j := range d.jobs {
		if j.State == "done" {
			continue
		}
		bg, why := d.background(j.Project, now)
		if bg != j.Background && !seen[j.Project] {
			if bg {
				logf("%s: now background (%s): its jobs wait behind project work", j.Project, why)
			} else {
				logf("%s: no longer background", j.Project)
			}
		}
		seen[j.Project] = true
		j.Background = bg
	}
}
