package daemon

// HTTP actions on existing jobs.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

type Act struct {
	PID       int     `json:"pid"`
	Exit      int     `json:"exit"`
	Minutes   float64 `json:"minutes"`
	Log       string  `json:"log"`
	Tail      string  `json:"tail,omitempty"` // done: the last part of the job's output, to tell why it failed
	Container string  `json:"container"`
	// Project is the caller's project: cancel, bid and extend on another project's job need Force.
	// A guard against agents touching each other's jobs by accident, not a security boundary.
	Project string `json:"project,omitempty"`
	Force   bool   `json:"force,omitempty"`
	Engine  string `json:"engine,omitempty"` // container engine: docker | podman
	// amend: new declarations for a waiting job (0 = unchanged).
	RAMGB  float64 `json:"ram_gb,omitempty"`
	VRAMGB float64 `json:"vram_gb,omitempty"`
	Urgent bool    `json:"urgent,omitempty"`
}

// validID: ticket ids end up in file names, so only what NewID makes is accepted.
func validID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (d *Daemon) act(w http.ResponseWriter, r *http.Request) {
	var a Act
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			fail(w, 400, "bad body: "+err.Error())
			return
		}
	}
	caller := callerPID(r)
	callerAgent, callerHuman := d.sessionOf(caller)
	callerAgentAt := sysinfo.Created(callerAgent)
	d.mu.Lock()
	defer d.mu.Unlock()
	id, verb := r.PathValue("id"), r.PathValue("verb")
	j, ok := d.jobs[id]
	if !ok {
		fail(w, 404, "no such job "+id)
		return
	}
	now := time.Now()
	// Who may change a job: the human (no agent above the caller: a terminal, the VS Code button),
	// the agent session that submitted it, or an agent in the same project. Other agents need
	// --force. A guard against agents touching each other's work by accident, not a sandbox.
	if verb == "cancel" || verb == "bid" || verb == "amend" || verb == "extend" || verb == "done" {
		sameSession := callerAgent != 0 && (callerAgent == j.ownerAgent && callerAgentAt == j.ownerCreated ||
			caller == j.ClientPID) // the job's own client always may (an unknown agent's own job)
		if !callerHuman && j.human && !sameSession && !a.Force {
			fail(w, http.StatusForbidden, fmt.Sprintf("job %s is the human's; add --force if you mean it", id))
			return
		}
		if !callerHuman && !sameSession && cleanText(a.Project, 100) != j.Project && !a.Force {
			fail(w, http.StatusForbidden, fmt.Sprintf("job %s belongs to another agent session (project %s); add --force if you mean it", id, j.Project))
			return
		}
	}
	if caller == 0 {
		fail(w, http.StatusForbidden, "couldn't tell which process is asking (half-closed or foreign connection)")
		return
	}
	if verb == "done" && j.ClientPID > 1 && caller != j.ClientPID && !callerHuman && !a.Force {
		// Closing a job someone else is running would leave its process ungoverned: cancel it instead.
		fail(w, http.StatusForbidden, "only the job's own client reports it done; use cancel to stop it")
		return
	}
	switch verb {
	case "start":
		if j.State != "granted" {
			fail(w, http.StatusConflict, "job "+id+" is "+j.State+", not granted")
			return
		}
		if a.PID <= 1 || a.PID > math.MaxInt32 { // larger values wrap when cast for the OS call
			fail(w, 400, fmt.Sprintf("pid %d isn't a process", a.PID))
			return
		}
		starter := caller
		if starter < 0 {
			fail(w, http.StatusServiceUnavailable, "couldn't tell which process is asking; try again")
			return
		}
		if j.ClientPID > 1 && starter != j.ClientPID {
			fail(w, http.StatusForbidden, "only the client that queued this job can start it")
			return
		}
		if !sysinfo.Alive(a.PID) {
			// It already finished (a quick command beats its own start report): nothing to govern;
			// its client reports done next.
			j.State, j.Started = "running", now
			break
		}
		if starter <= 1 || !sysinfo.DescendantOf(a.PID, starter) {
			// The job is a process its client started. Anything else (a game, a browser, another
			// agent) must not be made idle-priority and killed with this job.
			fail(w, http.StatusForbidden, fmt.Sprintf("pid %d wasn't started by this job's client", a.PID))
			return
		}
		j.State, j.Started, j.PID = "running", now, a.PID
		j.rootCreated = sysinfo.Created(a.PID)
		j.pids = map[int]int64{a.PID: j.rootCreated}
		if j.price == 0 {
			d.lockPrice(j, now)
		}
		if err := sysinfo.Background(a.PID); err != nil {
			logf("%s: background priority: %v", j.Name, err)
		}
		logf("%s: started pid %d after %.1f min", j.Name, a.PID, now.Sub(j.Created).Minutes())
		d.st.Open = d.openJobs(now) // now: a daemon that dies before the next save still knows it
		_ = d.st.write()
	case "container":
		if a.Container == "" || caller < 0 || (j.ClientPID > 1 && caller != j.ClientPID) {
			fail(w, http.StatusForbidden, "only the job's own client can name its container")
			return
		}
		if a.Container != j.container {
			// Verified before the lock below would matter: only a container labeled for this job.
			d.mu.Unlock()
			full := containerFor(a.Engine, a.Container, j.ID)
			d.mu.Lock()
			if full == "" {
				fail(w, http.StatusForbidden, "container "+a.Container+" isn't labeled firefighter.job="+j.ID)
				return
			}
			if j.container != "" && !strings.HasPrefix(full, j.container) && !strings.HasPrefix(j.container, full) {
				// Write-once: a job's container is never swapped for another one (review 11/20).
				fail(w, http.StatusConflict, "job "+id+" already has its container")
				return
			}
			if j.State == "done" || !j.killedAt.IsZero() {
				d.docker(a.Engine, "kill", full) // stopped while we looked: its container goes too
				fail(w, http.StatusConflict, "job "+id+" was stopped")
				return
			}
			// Later pause/kill calls use the full id: a name or prefix could point at another
			// container once this one is gone.
			j.container, j.engine, j.ctrSeen = full, a.Engine, ""
			// A container whose --memory is above what was reserved for it is held to the
			// reservation: past it, the container's own OOM killer acts, not Windows' paging.
			// One with no --memory at all could otherwise grow until Windows pages (review 11/20).
			// Not on a guessed need (no --ram, no --memory): that OOM-kills real work.
			if dm := dockerMemGB(j.Command); j.NeedRAM > 0 && ((dm == 0 && j.RAMGB > 0) || dm > j.NeedRAM+0.05) {
				lim := fmt.Sprintf("%dm", int(j.NeedRAM*1024))
				d.docker(a.Engine, "update", "--memory", lim, "--memory-swap", lim, full)
				asked := "no --memory"
				if dm > 0 {
					asked = fmt.Sprintf("--memory %.1f GB", dm)
				}
				logf("%s: container limited to %.1f GB (it asked %s; that is what firefighter reserved for it)", j.Name, j.NeedRAM, asked)
			}
		}
	case "amend": // new declarations for a waiting job: same id, same client, same place in line
		if j.State != "waiting" {
			fail(w, http.StatusConflict, "job "+id+" is "+j.State+"; amend only changes a waiting job")
			return
		}
		if a.RAMGB < 0 || a.VRAMGB < 0 || a.Minutes < 0 || (d.cfg.Ladder.MaxLeaseMinutes > 0 && a.Minutes > d.cfg.Ladder.MaxLeaseMinutes) || (a.VRAMGB > 0 && !j.GPU) {
			fail(w, 400, "amend: --ram/--vram in GB (--vram only for a GPU job), --minutes up to the longest lease")
			return
		}
		var changed []string
		if a.RAMGB > 0 {
			changed = append(changed, fmt.Sprintf("ram %.1f -> %.1f GB", j.RAMGB, a.RAMGB))
			j.RAMGB = a.RAMGB
		}
		if a.VRAMGB > 0 {
			changed = append(changed, fmt.Sprintf("vram %.1f -> %.1f GB", j.VRAMGB, a.VRAMGB))
			j.VRAMGB = a.VRAMGB
		}
		if a.Minutes > 3 && j.Quick {
			a.Minutes = 3 // a quick look stays one
		}
		if a.Minutes > 0 {
			changed = append(changed, fmt.Sprintf("minutes %.0f -> %.0f", j.Minutes, a.Minutes))
			j.Minutes = a.Minutes
		}
		if a.Urgent && j.Bid < policy.BidUrgent {
			changed = append(changed, "urgent")
			j.Bid, j.Spot = policy.BidUrgent, false
			j.Band = policy.BandFor(j.Ticket, d.account(j.Project, now).Left(d.cfg, d.cfg.Allowance(j.Project)) > 0)
		}
		if len(changed) == 0 {
			fail(w, 400, "amend: nothing to change (--ram, --vram, --minutes, --urgent)")
			return
		}
		d.fillNeeds(j.Ticket)
		logf("%s: amended (%s); now needs %.1f GB RAM %.1f GB VRAM", j.Name, strings.Join(changed, ", "), j.NeedRAM, j.NeedVRAM)
	case "bid": // re-bid while waiting, after reading the market
		if a.Minutes < 1 || a.Minutes > 10 {
			fail(w, 400, "bid must be 1-10")
			return
		}
		if j.State != "waiting" {
			fail(w, http.StatusConflict, "job "+id+" is "+j.State+"; a bid only changes a waiting job")
			return
		}
		{
			j.Bid = max(1, min(10, int(a.Minutes)))
			if j.Spot && j.Bid >= policy.BidNormal {
				j.Spot = false // asking for a normal turn: its band follows the budget again
				j.Band = policy.BandFor(j.Ticket, d.account(j.Project, now).Left(d.cfg, d.cfg.Allowance(j.Project)) > 0)
			}
			logf("%s: re-bid %d", j.Name, j.Bid)
		}
	case "extend":
		if a.Minutes <= 0 || a.Minutes > 24*60 {
			fail(w, 400, "extend by 1 to 1440 minutes")
			return
		}
		if j.Quick {
			fail(w, http.StatusConflict, "a --quick job can't be extended (it is a look of 3 minutes at most); run the rest as a normal job")
			return
		}
		if j.backfilled {
			fail(w, http.StatusConflict, "this job started early (backfill) on a short lease; it can't be extended. Requeue the rest with an honest --minutes")
			return
		}
		if lim := d.cfg.Ladder.MaxLeaseMinutes; lim > 0 && j.Minutes+j.extended+a.Minutes > lim {
			fail(w, http.StatusConflict, fmt.Sprintf("a lease can't pass %.0f minutes; requeue the rest", lim))
			return
		}
		// An extension pushes the lease out at today's price (no keeping a 0.5x idle-hour price for
		// a day), and an overrun already charged at 2x stays that way.
		j.extended += a.Minutes
		d.lockPrice(j, now)
		logf("%s: lease extended by %.0f min", j.Name, a.Minutes)
	case "done":
		outcome := "ok"
		if a.Exit != 0 {
			outcome = "failed"
		}
		if !j.checkpoint.IsZero() || a.Exit == 75 { // 75 = checkpointed, requeue me (EX_TEMPFAIL)
			outcome = "evicted"
		}
		if outcome == "failed" && j.killedAt.IsZero() {
			// It failed on its own: say why and what to change; out of VRAM or RAM, its learned
			// need goes up so the next run is admitted with room (review 11/20, after OpenPAI).
			if f := policy.ClassifyExit(a.Exit, a.Tail); f != nil {
				j.failure = f
				switch f.Kind {
				case "vram":
					j.vramOOM = true
				case "ram":
					j.oom = true
				}
				logf("%s: failed: %s (%s)", j.Name, f.Why, f.Kind)
			}
		}
		// Shown to other agents in status: one short, printable line, like every text an agent sends.
		d.finish(id, a.Exit, outcome, cleanText(a.Log, 500))
	case "cancel":
		if j.State == "running" {
			j.reason = "cancelled"
			d.killJob(j, now)
		}
		d.finish(id, -1, "cancelled", "")
	default:
		fail(w, 404, "unknown action "+verb)
		return
	}
	d.broadcast()
	reply(w, d.view(j))
}
