package daemon

// Announcements of upcoming work and their ownership.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

func (d *Daemon) plan(w http.ResponseWriter, r *http.Request) {
	var p Plan
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if p.Minutes <= 0 || p.Minutes > 7*24*60 || p.GPUPct < 0 || p.GPUPct > 100 || p.VRAMGB < 0 || p.VRAMGB > 4096 ||
		p.RAMGB < 0 || p.RAMGB > 4096 || len(p.What) > 300 || len(p.When) > 40 || len(p.Project) > 200 {
		fail(w, 400, "plan: minutes 1-10080, gpu 0-100, sizes 0-4096 GB, short text")
		return
	}
	p.Project, p.What, p.When = cleanText(p.Project, 100), cleanText(p.What, 300), cleanText(p.When, 40)
	planCaller := callerPID(r)
	if planCaller == 0 {
		fail(w, http.StatusForbidden, "couldn't tell which process is asking")
		return
	}
	planOwner, planHuman := d.sessionOf(planCaller)
	if planHuman {
		planOwner = -1 // the human's: only the human removes it
	}
	planOwnerAt := sysinfo.Created(planOwner)
	d.mu.Lock()
	defer d.mu.Unlock()
	mine := 0
	for _, o := range d.st.Plans {
		if o.Project == p.Project {
			mine++
		}
	}
	if !planHuman && (mine >= 10 || len(d.st.Plans) >= 100) {
		fail(w, http.StatusTooManyRequests, "too many plans; remove some with `firefighter plan --done`")
		return
	}
	p.ID, p.Added = NewID(), time.Now()
	p.Owner, p.OwnerAt = planOwner, planOwnerAt
	d.st.Plans = append(d.st.Plans, &p)
	reply(w, p)
}

func (d *Daemon) unplan(w http.ResponseWriter, r *http.Request) {
	caller := callerPID(r)
	if caller == 0 {
		fail(w, http.StatusForbidden, "couldn't tell which process is asking")
		return
	}
	agent, human := d.sessionOf(caller)
	agentAt := sysinfo.Created(agent)
	d.mu.Lock()
	defer d.mu.Unlock()
	id := r.PathValue("id")
	var keep []*Plan
	removed := 0
	for _, p := range d.st.Plans {
		// The human removes any plan; an agent removes its own session's (or, after a restart
		// forgot who added it, its project's).
		// Like jobs: the human, the session that announced it, or its project (by project name).
		// A plan id after a restart forgot who added it: whoever names the id.
		if p.Owner == -1 && !human {
			keep = append(keep, p) // the human's plan
			continue
		}
		// Naming a project is not ownership (review 11/20: any agent could remove another's plans by
		// passing its project); only an ownerless plan (its owner forgotten in a restart, or exited)
		// goes to whoever names it.
		// A plan whose owner process has exited (a one-off script, a CI step) is ownerless too.
		gone := p.Owner > 0 && (!sysinfo.Alive(p.Owner) || sysinfo.Created(p.Owner) != p.OwnerAt)
		mine := human || (p.Owner > 0 && p.Owner == agent && p.OwnerAt == agentAt) || p.Owner == 0 || gone
		if (p.ID == id || p.Project == id) && mine {
			removed++
			continue
		}
		keep = append(keep, p)
	}
	d.st.Plans = keep
	reply(w, map[string]int{"plans": len(keep), "removed": removed})
}

// Plan is an announcement of heavy work coming later; it reserves nothing, it tells others.
type Plan struct {
	ID      string    `json:"id"`
	Project string    `json:"project"`
	What    string    `json:"what"`
	When    string    `json:"when"`
	Minutes float64   `json:"minutes"`
	GPUPct  float64   `json:"gpu_pct"`
	VRAMGB  float64   `json:"vram_gb"`
	RAMGB   float64   `json:"ram_gb"`
	Added   time.Time `json:"added"`
	Owner   int       `json:"owner,omitempty"`    // agent session that announced it; -1: the human
	OwnerAt int64     `json:"owner_at,omitempty"` // its start time (a reused pid isn't the same session)
}
