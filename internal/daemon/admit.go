package daemon

// Job admission, queue ordering, and capacity accounting.

import (
	"math"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
)

// backoff: how often in a row a job was stopped for the human, and when it last was.
type backoff struct {
	n   int
	at  time.Time
	why string // what it was stopped for, with the numbers
}

// backoffLeft is how long a requeued job still waits after its last eviction (0: none).
func (d *Daemon) backoffLeft(id string, now time.Time) time.Duration {
	b, ok := d.backoff[id]
	if !ok {
		return 0
	}
	wait := time.Duration(math.Min(30, math.Pow(2, float64(b.n-1)))) * time.Minute
	return max(0, wait-now.Sub(b.at))
}

// backingOff says whether a requeued job still waits out its back-off: 1, 2, 4, ... up to 30
// minutes after its last eviction, so a job that can't fit beside the human doesn't cycle.
func (d *Daemon) backingOff(id string, now time.Time) bool {
	b, ok := d.backoff[id]
	if !ok {
		return false
	}
	wait := time.Duration(math.Min(30, math.Pow(2, float64(b.n-1)))) * time.Minute
	return now.Sub(b.at) < wait
}

// shortJob: a job that may backfill ahead of a blocked head. Both what it declares and what its
// history predicts must be short (history alone can be skewed by one quick run of the command).
func shortJob(t *policy.Ticket) bool {
	return t.Minutes > 0 && t.Minutes <= 10 && t.PredMinutes <= 10
}

func (d *Daemon) capacity() policy.Capacity {
	_, vt := d.snap.MaxFreeVRAM()
	return policy.Capacity{CPUs: float64(max(1, d.snap.CPUs)), RAMGB: math.Max(1, d.snap.RAMTotalGB), VRAMGB: vt}
}

func (d *Daemon) account(project string, now time.Time) *policy.Account {
	a := d.st.Accounts[project]
	if a == nil {
		na := d.newAccount(project, now)
		if len(d.st.Accounts) > 0 {
			if d.cfg.Budget.Policy == "daily" {
				// Daily budgets: a new project's first day is what's left in the shared pool.
				grant := math.Min(d.cfg.Allowance(project), d.newcomers(now))
				na.Day, na.UsedDay = now.Format("2006-01-02"), d.cfg.Allowance(project)-grant
				d.st.Newcomers -= grant
			} else {
				d.st.Newcomers -= na.Bucket // drawn from the pool all new projects share
			}
		}
		a = &na
		d.st.Accounts[project] = a
	}
	a.Decay(now, d.cfg.Order.HalfLifeHours)
	before := a.Bucket
	a.Refill(now, d.cfg, d.cfg.Allowance(project))
	// A project in its first day refills from the pool all new projects share, so a stream of
	// invented names can't each earn the full daily rate.
	if !a.Created.IsZero() && now.Sub(a.Created) < 24*time.Hour && len(d.st.Accounts) > 1 {
		// Only what lifts it above zero: paying off a debt mints nothing, and a project whose first
		// job overdrew it must not sit frozen in debt for a day because the pool is empty.
		if gain := a.Bucket - math.Max(before, 0); gain > 0 {
			got := math.Min(gain, d.newcomers(now))
			a.Bucket -= gain - got
			d.st.Newcomers -= got
		}
	}
	return a
}

// ---- admission ----------------------------------------------------------------------------------

func (d *Daemon) queued() []*policy.Ticket {
	var waiting []*policy.Ticket
	for _, j := range d.jobs {
		if j.State == "waiting" {
			waiting = append(waiting, j.Ticket)
		}
	}
	return waiting
}

func (d *Daemon) waiting() []*policy.Ticket {
	return policy.Order(d.cfg, d.queued(), d.st.Accounts, d.capacity(), time.Now(), d.rng)
}

func (d *Daemon) admit(now time.Time) {
	// Just after a restart, jobs the previous guard was running re-register within seconds: no
	// grant until they have (or 15 s have passed), so a waiting job can't take a slot one of them
	// holds (2026-11-22: three GPU jobs on a card with gpu.max_jobs 2 after the restart).
	if now.Sub(d.started) < 15*time.Second {
		for id := range d.prevOpen {
			if _, back := d.jobs[id]; !back {
				return
			}
		}
	}
	ctx := d.newAdmitCtx(now)
	var waiting []*policy.Ticket
	if ctx.updating {
		waiting = d.queued()
	} else {
		waiting = d.waiting()
		if d.cfg.Order.Policy == "lottery" {
			d.admissionDraw = waiting
		} else {
			d.admissionDraw = nil
		}
	}
	changed := false
	for _, t := range waiting {
		j := d.jobs[t.ID]
		decision := d.gate(j, ctx)
		if decision.Kind != blockNone {
			continue
		}
		ctx.ram -= t.NeedRAM
		ctx.active++
		if d.st.History[histKey(t)] == nil {
			if d.unknownAt == nil {
				d.unknownAt = map[bool]time.Time{}
			}
			d.unknownAt[t.GPU] = now
		}
		if t.GPU {
			ctx.vram -= t.NeedVRAM
			ctx.gpuRunning++
			j.backfilled = decision.backfilled
			if len(d.snap.GPUs) > 1 {
				j.device = decision.Card
			}
			if need := d.needOn(t, decision.Card); need < t.NeedVRAM-0.05 {
				logf("%s: needs %.1f GB, more than this card ever frees; starting at its emptiest (%.1f GB)", j.Name, t.NeedVRAM, need)
			}
		}
		j.State, j.granted = "granted", now
		d.lockPrice(j, now)
		logf("%s: granted (gpu jobs %d of %d, %.1f GB RAM / %.1f GB VRAM headroom left)", j.Name, ctx.gpuRunning, d.cfg.GPU.MaxJobs, ctx.ram, ctx.vram)
		changed = true
	}
	if !ctx.updating {
		d.ramBlocked = ctx.ramBlocked
	}
	if changed {
		d.broadcast()
	}
}
