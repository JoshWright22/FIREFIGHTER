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

func (d *Daemon) waiting() []*policy.Ticket {
	var w []*policy.Ticket
	for _, j := range d.jobs {
		if j.State == "waiting" {
			w = append(w, j.Ticket)
		}
	}
	return policy.Order(d.cfg, w, d.st.Accounts, d.capacity(), time.Now(), d.rng)
}

func (d *Daemon) admit(now time.Time) {
	if d.updating.Load() {
		return // restarting as a newer version in a moment: nothing starts on this guard
	}
	c := d.cfg
	ram, vram := d.headroom(now)

	_, vt := d.snap.MaxFreeVRAM()
	gpuRunning := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") {
			gpuRunning++
		}
	}
	gpuBlocked := d.gpuHeld()
	if len(d.snap.GPUs) == 0 {
		// No card readings: one count for the machine. Pack a second GPU job only while the
		// smoothed (not momentary) utilization leaves room.
		gpuRunning = d.gpuBusySlots()
		if gpuRunning >= c.GPU.MaxJobs || (gpuRunning > 0 && math.Max(d.utilEWMA, d.snap.GPUUtil()*0.5) >= c.GPU.PackBelowUtil) {
			gpuBlocked = true
		}
	}
	changed := false
	headWaitsOnVRAM, cpuHeadBlocked := map[int]bool{}, false // per card
	// RAM: a job starts only if its need plus a margin fits beside the reserve, and never while RAM
	// is (or just was) short: a grant with no room to spare is a job stopped minutes later.
	margin := d.ramMargin()
	settling := d.ramSettling(now) != ""
	gpuHeadRAM := false // an overdue GPU job waits for RAM: only short jobs may start ahead of it
	d.ramBlocked = false
	if d.unknownAt == nil {
		d.unknownAt = map[bool]time.Time{}
	}
	jobsOn := func(gpu bool) int {
		n := 0
		for _, o := range d.jobs {
			if (o.State == "running" || o.State == "granted") && (!gpu || o.GPU) {
				n++
			}
		}
		return n
	}
	for _, t := range d.waiting() {
		j := d.jobs[t.ID]
		overdue := now.Sub(t.Created).Minutes() >= c.Order.MaxWaitMinutes
		if d.st.History[histKey(t)] == nil && now.Sub(d.unknownAt[t.GPU]) < 5*time.Second {
			continue // first-time commands (no learned peaks) start 5 s apart, so their real use shows
		}
		if t.Net && !d.netFree() && !(overdue && d.netHumanOnly()) {
			continue // network turn taken, a game holds the machine, or the human is using the link
		}
		if d.backingOff(t.ID, now) {
			continue // stopped for the human recently: give it a while before trying again
		}
		if t.GPU {
			if gpuBlocked {
				continue
			}
			if d.gpuDegraded {
				continue // fewer GPUs answered than usual: no GPU grant on a partial reading
			}
			if gpuHeadRAM && !shortJob(t) {
				continue // the GPU stays busy with short jobs that fit; long ones would starve the head of RAM
			}
			card, left := d.cardFor(t) // already less the jobs granted this tick
			if card < 0 && len(d.snap.GPUs) > 0 {
				continue // no card of the vendor it needs right now
			}
			if card >= 0 && !d.cardOpen(card) {
				continue // every card it can use has its turns taken (gpu.max_jobs each) or is busy
			}
			if card >= 0 && d.cardJobs(card) > 0 && t.PredGPU > 0 && d.utilOf(card)+t.PredGPU > 110 {
				// Gandiva packs two jobs on a card only when their loads fit together: this one's
				// past runs say it would push the card past full, so it takes a turn instead.
				continue
			}
			left -= d.res.VRAMGB
			need := d.needOn(t, card)
			if d.waitsForAway(t) {
				continue
			}
			if vt > 0 && card >= 0 && need > left {
				headWaitsOnVRAM[card] = true // the head waits for this card's VRAM; only short jobs may backfill it
				continue
			}
			// EASY-style backfill: behind a head that waits on VRAM, only a short job that fits
			// now may start, so the head is delayed by at most ~10 minutes.
			if headWaitsOnVRAM[card] && !shortJob(t) {
				continue
			}
		} else if cpuHeadBlocked && !shortJob(t) {
			continue // an overdue big CPU job is waiting for RAM: don't let long jobs eat it first
		}
		if d.diskWhy(j) != "" {
			continue // its drive is under the human's disk reserve, or lacks room for what it writes
		}
		if d.cpuWhy(j) != "" {
			continue // the CPUs outside the human's reserve are taken by jobs already running
		}
		// Short of RAM. An overdue job with nothing else running needs no margin (the ladder still
		// protects the human); otherwise it holds the line only once it's overdue.
		lone := overdue && jobsOn(false) == 0 && t.NeedRAM <= ram
		if settling || (t.NeedRAM+margin > ram && !lone) {
			d.ramBlocked = true
			if overdue {
				if t.GPU {
					gpuHeadRAM = true
				} else {
					cpuHeadBlocked = true
				}
			}
			continue
		}
		ram -= t.NeedRAM
		if d.st.History[histKey(t)] == nil {
			d.unknownAt[t.GPU] = now
		}
		if t.GPU {
			card, _ := d.cardFor(t)
			vram -= t.NeedVRAM
			gpuRunning++
			if len(d.snap.GPUs) == 0 && gpuRunning >= c.GPU.MaxJobs {
				gpuBlocked = true // no card readings: one machine-wide count, and it's now full
			}
			j.backfilled = headWaitsOnVRAM[card] || gpuHeadRAM
			if len(d.snap.GPUs) > 1 {
				j.device = card // and the client pins it there, in that vendor's numbering
			}
		}
		if t.GPU {
			if card, _ := d.cardFor(t); d.needOn(t, card) < t.NeedVRAM-0.05 {
				logf("%s: needs %.1f GB, more than this card ever frees; starting at its emptiest (%.1f GB)", j.Name, t.NeedVRAM, d.needOn(t, card))
			}
		}
		j.State, j.granted = "granted", now
		d.lockPrice(j, now)
		logf("%s: granted (gpu jobs %d of %d, %.1f GB RAM / %.1f GB VRAM headroom left)", j.Name, gpuRunning, c.GPU.MaxJobs, ram, vram)
		changed = true
	}
	if changed {
		d.broadcast()
	}
}
