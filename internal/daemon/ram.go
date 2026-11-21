package daemon

// RAM pressure: VM cache reclaim, job holds, and headroom.

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// headroom is what jobs may still take: available minus the reserve minus what granted and running
// jobs haven't grown into yet (a job loading data for minutes hasn't allocated its model; a job
// that freed its cache while paused will take it back). VRAM is per card: the freest GPU, less the
// unclaimed needs of jobs placed on it.
func (d *Daemon) headroom(now time.Time) (ram, vram float64) {
	ram = d.snap.RAMAvailGB - d.ramFloor()
	// Windows commit: a job's allocations are promised against RAM + pagefile. Near the limit
	// Windows grows the pagefile and pages (41.1 of 44.1 GB on 2026-11-20, as the desktop froze), so
	// the commit left beyond reserve.commit_free_gb bounds what a new job may take too.
	if cf := d.snap.CommitFreeGB(); cf >= 0 && d.cfg.Reserve.CommitFreeGB > 0 {
		ram = math.Min(ram, cf-d.cfg.Reserve.CommitFreeGB)
	}
	card, free := d.freestCard()
	vram = free - d.res.VRAMGB
	for _, j := range d.jobs {
		if j.State == "granted" || j.State == "running" {
			ram -= d.reservedRAM(j)
			if j.GPU && d.onCard(j, card) {
				vram -= math.Max(0, j.NeedVRAM-j.vramGB)
			}
		}
	}
	return
}

// ramFloor is the free RAM kept for the human: the learned (or configured) reserve, and never less
// than reserve.min_free_gb (at most 15% of RAM). Admission keeps it free beside every job's full
// need, and the RAM ladder acts as soon as free RAM falls under it.
func (d *Daemon) ramFloor() float64 {
	total := d.snap.RAMTotalGB
	return math.Max(total*d.res.RAMPct/100, math.Min(d.cfg.Reserve.MinFreeGB, total*0.15))
}

// ramMost is the most RAM a job can ever count on: the machine's, less what is always in use
// outside jobs, less the reserve when the human is away, less the admission margin. A need above
// it could never be met, so needs are cut to it (fillNeeds) and the job starts when the machine is
// at its emptiest.
func (d *Daemon) ramMost() float64 {
	total := d.snap.RAMTotalGB
	most := total * (1 - d.cfg.Reserve.Away.RAMPct/100) * 0.9
	if seen := total*(1-d.cfg.Reserve.Away.RAMPct/100) - d.ramBase - d.ramMargin(); d.ramBase > 0 && seen < most {
		most = seen
	}
	return math.Max(most, math.Min(1, total/4))
}

// reservedRAM is the RAM still held back for a granted or running job beyond what it uses now.
// It is Borg's reservation (Verma et al., EuroSys 2015, section 5.5): equal to the request at
// first, then "after 300 s, to allow for startup transients, it decays slowly towards the actual
// usage plus a safety margin", and it rises at once when the job uses more. Here the gap between
// the need and the job's own peak + 15% halves with every 10 minutes of work, so a job that asked
// for 12 GB and settled at 7 doesn't keep the next job out for hours.
func (d *Daemon) reservedRAM(j *job) float64 {
	need := j.NeedRAM
	if j.held == "ram" {
		return math.Max(0, math.Max(need, j.heldRAM)-j.ramGB) // suspended to free RAM: it takes this back when it resumes
	}
	if seen := j.peakRAM * (1 + usage.SafetyMargin); j.State == "running" && j.workSec > 300 && need > seen {
		need = seen + (need-seen)*math.Pow(0.5, (j.workSec-300)/600)
	}
	return math.Max(0, need-j.ramGB)
}

// ---- RAM and disk pressure --------------------------------------------------------------------------

const (
	reclaimMinGB = 1.0              // a VM cache smaller than this isn't worth a reclaim
	reclaimEvery = 60 * time.Second // between reclaims (doubled each time one returns next to nothing)
	holdGap      = 5 * time.Second  // between two suspensions, so each one's effect is seen first
	minHoldRAM   = 0.25             // GB: a job holding less isn't suspended or stopped for RAM
)

// reclaimState is one request to the local Linux VM to give its file cache back to the host.
type reclaimState struct {
	on     bool
	at     time.Time // when it started
	end    time.Time // when the last one finished
	before float64   // free RAM when it started
	misses int       // reclaims in a row that returned next to nothing
}

// refreshVM reads the local Linux VM's memory in the background every 5 s: what the host gave it
// always (from the process list), its own numbers only while RAM is tight (wanted), because asking
// runs a command inside the VM and would keep an idle one awake.
func (d *Daemon) refreshVM(now time.Time, wanted int) {
	if d.vmBusy.Load() || now.Sub(d.vmProbeAt) < 5*time.Second {
		return
	}
	d.vmBusy.Store(true)
	d.vmProbeAt = now
	// The guest is asked every 5 s while RAM is tight, every 20 s while a job only waits for RAM.
	guest := wanted == 2 || (wanted == 1 && now.Sub(d.vmGuestAt) >= 20*time.Second)
	if guest {
		d.vmGuestAt = now
	}
	go func() {
		defer d.vmBusy.Store(false)
		vm := sysinfo.VM{HostGB: sysinfo.VMHost()}
		if !guest && vm.HostGB >= reclaimMinGB {
			// Not asked this round: the last reading of the guest stands while it is fresh.
			d.mu.Lock()
			if d.vm.Guest && time.Since(d.vmAt) < 30*time.Second {
				d.vm.HostGB = vm.HostGB
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
		}
		if guest && vm.HostGB >= reclaimMinGB {
			if g, ok := sysinfo.VMGuest(vm.HostGB); ok {
				vm = g
			}
		}
		d.mu.Lock()
		d.vm, d.vmAt = vm, time.Now()
		d.mu.Unlock()
	}()
}

// reclaimable is the RAM the local Linux VM would give back if asked (0 when unknown or stale).
func (d *Daemon) reclaimable(now time.Time) float64 {
	if !d.cfg.Reserve.ReclaimVMCache || !d.vm.Guest || now.Sub(d.vmAt) > 30*time.Second {
		return 0
	}
	return d.vm.CacheGB
}

// reclaimVM asks the local Linux VM to give its file cache back to the host, in the background. It
// is the first answer to a RAM shortage: it frees memory nobody needs, and no job is touched while
// it runs. urgent: RAM is under the reserve (otherwise a job is only waiting for RAM, and the VM is
// asked five times less often). Returns whether a reclaim is now under way.
func (d *Daemon) reclaimVM(now time.Time, why string, urgent bool) bool {
	if d.reclaim.on {
		return true
	}
	wait := reclaimEvery * time.Duration(1<<min(d.reclaim.misses, 4))
	if !urgent {
		wait *= 5 // only a waiting job wants it: a running build re-reads what it loses, so not every minute
	}
	cache := d.reclaimable(now)
	if cache < reclaimMinGB || now.Sub(d.reclaim.end) < wait {
		return false
	}
	vm := d.vm
	d.reclaim.on, d.reclaim.at, d.reclaim.before = true, now, d.snap.RAMAvailGB
	logf("RAM: %s. The WSL/Docker VM (%s) holds %.1f GB but uses %.1f GB; asking it to give back its %.1f GB of file cache before any job is touched",
		why, vm.Name, vm.HostGB, vm.UsedGB, cache)
	d.broadcast()
	go func() {
		err := sysinfo.ReclaimVM(vm.Name)
		// The pages come back to the host over some seconds: wait until free RAM stops growing.
		after := sysinfo.AvailGB()
		for i := 0; err == nil && i < 15; i++ {
			time.Sleep(2 * time.Second)
			next := sysinfo.AvailGB()
			grew := next - after
			after = next
			if grew < 0.2 {
				break
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		end := time.Now()
		gained := after - d.reclaim.before
		d.reclaim.on, d.reclaim.end = false, end
		d.vm.Guest = false // read it again before counting on any more
		switch {
		case err != nil:
			d.reclaim.misses++
			logf("RAM: the VM's cache could not be reclaimed: %v", err)
		case gained < 0.5:
			d.reclaim.misses++
			logf("RAM: reclaim returned next to nothing (free RAM %.1f -> %.1f GB in %.0f s)", d.reclaim.before, after, end.Sub(d.reclaim.at).Seconds())
		default:
			d.reclaim.misses = 0
			logf("RAM: reclaimed %.1f GB from the WSL/Docker VM in %.0f s (free RAM %.1f -> %.1f GB); no job was stopped", gained, end.Sub(d.reclaim.at).Seconds(), d.reclaim.before, after)
		}
		d.broadcast()
	}()
	return true
}

// ramWhy says why RAM is short, with the numbers: free RAM, the floor and what sets it, and what
// the WSL VM holds (its file cache is RAM no Windows program shows).
func (d *Daemon) ramWhy(avail, floor float64) string {
	total := d.snap.RAMTotalGB
	what := fmt.Sprintf("the human's %.0f%% reserve", d.res.RAMPct)
	if pct := total * d.res.RAMPct / 100; floor > pct+0.01 {
		what = fmt.Sprintf("the %.1f GB minimum (reserve.min_free_gb; the human's %.0f%% reserve is %.1f GB)", floor, d.res.RAMPct, pct)
	}
	return fmt.Sprintf("RAM: %.1f GB free, %.1f GB under the %.1f GB floor kept for the human (%s)%s", avail, floor-avail, floor, what, d.vmNote())
}

// vmNote: what the WSL/Docker VM holds, when that is a large part of the story ("" otherwise).
func (d *Daemon) vmNote() string {
	if d.vm.HostGB < 2 {
		return ""
	}
	if d.vm.Guest {
		return fmt.Sprintf("; the WSL/Docker VM holds %.1f GB, %.1f GB of it file cache", d.vm.HostGB, d.vm.CacheGB)
	}
	return fmt.Sprintf("; the WSL/Docker VM holds %.1f GB", d.vm.HostGB)
}

// reclaimCritical asks the VM for its cache when RAM is critical and the VM holds memory, even when
// its own numbers aren't in, are stale, or the last reclaims returned little: the VM can fill with
// cache within a minute (2026-11-20: 2.1 GB at 07:13, about 11 GB by 07:20, and no second reclaim
// came while Windows sat at 0.8 GB free). At most every 30 s.
func (d *Daemon) reclaimCritical(now time.Time, avail float64) bool {
	if !d.cfg.Reserve.ReclaimVMCache || d.vm.HostGB < 2 || now.Sub(d.reclaim.end) < 30*time.Second {
		return false
	}
	saved := d.vm
	if !d.vm.Guest || now.Sub(d.vmAt) > 30*time.Second || d.vm.CacheGB < reclaimMinGB {
		// Unknown split: assume what the VM holds beyond 1 GB may be cache.
		d.vm = sysinfo.VM{Name: d.vm.Name, HostGB: d.vm.HostGB, UsedGB: 1, CacheGB: d.vm.HostGB - 1, Guest: true}
		d.vmAt = now
	}
	misses := d.reclaim.misses
	d.reclaim.misses, d.reclaim.end = 0, time.Time{}
	ok := d.reclaimVM(now, fmt.Sprintf("RAM critically low (%.1f GB free)", avail), true)
	if !ok {
		d.vm, d.reclaim.misses = saved, misses
	}
	return ok
}

// ramMargin is the RAM that must stay free beyond the reserve and a new job's own need.
func (d *Daemon) ramMargin() float64 {
	return d.snap.RAMTotalGB * d.cfg.Reserve.RAMMarginPct / 100
}

// ramSettling says why no job may start for RAM reasons right now: a reclaim is
// running, a job is suspended to free RAM (it comes back first), or RAM was under the reserve
// within the last reserve.ram_shrink_seconds.
func (d *Daemon) ramSettling(now time.Time) Decision {
	if d.reclaim.on {
		return blocked(blockRAM, "giving the WSL/Docker VM's file cache back to Windows first", false)
	}
	for _, j := range d.jobs {
		if j.State == "running" && j.held == "ram" {
			return blocked(blockRAM, j.Name+" is suspended to free RAM and resumes first", false)
		}
	}
	if ago := now.Sub(d.ramLowLast).Seconds(); !d.ramLowLast.IsZero() && ago < d.cfg.Reserve.RAMShrinkSeconds {
		if ago < 1 {
			return blocked(blockRAM, fmt.Sprintf("RAM is under the human's %.0f%% reserve (%.1f GB free)", d.res.RAMPct, d.snap.RAMAvailGB), false)
		}
		hold := blocked(blockRAM, fmt.Sprintf("RAM was under the human's reserve %.0f s ago: waiting %.0f s for it to settle", ago, d.cfg.Reserve.RAMShrinkSeconds), true)
		hold.retryIn = dur(d.cfg.Reserve.RAMShrinkSeconds) - now.Sub(d.ramLowLast)
		return hold
	}
	return Decision{}
}

// hold suspends a job to free a resource the human is short of. On Windows its RAM is also moved
// out to the pagefile, so suspending really frees it. The job loses nothing: it resumes where it
// stopped when there is room again (unhold).
func (d *Daemon) hold(j *job, what, why string, now time.Time) {
	if j.pausedAt.IsZero() {
		j.pausedAt = now
	}
	_ = os.Remove(PauseFile(j.ID)) // frozen, not asked to pause: nothing for it to answer
	j.held, j.heldAt, j.reason, j.okSince = what, now, why, time.Time{}
	j.holds++
	if j.suspended.IsZero() {
		j.suspended = now
		d.suspend(j)
	}
	if what == "ram" {
		j.heldRAM, j.freedAt = j.ramGB, now
		d.each(j, sysinfo.Trim)
		logf("%s: suspended, not stopped: %s. Its %.1f GB of RAM moves to the pagefile; it resumes when RAM is back", j.Name, why, j.ramGB)
	} else {
		logf("%s: suspended, not stopped: %s. It resumes when there is space again", j.Name, why)
	}
	d.lastHold = now
	d.broadcast()
}

// unhold lets a held job run again (or hands it back to the game rule, when a game still holds it).
func (d *Daemon) unhold(j *job, now time.Time, why string) {
	logf("%s: resumed after %.0f s suspended: %s", j.Name, now.Sub(j.heldAt).Seconds(), why)
	if d.heldForHuman(j) {
		j.heldTotal += now.Sub(j.heldAt)
		j.held, j.heldDrive, j.okSince = "", "", time.Time{}
		j.reason = "the human needs the GPU (" + d.why() + ")"
	} else {
		d.unpause(j, now)
	}
	d.lastHold = now
	d.broadcast()
}

// ramVictim picks the job to suspend for RAM: one not already held whose RAM can be moved out,
// preferring one that covers the shortfall alone, then the highest band, then the heaviest.
func (d *Daemon) ramVictim(need float64) *job {
	var best *job
	w := func(j *job) float64 { return j.ramGB }
	for _, j := range d.running() {
		if !j.killedAt.IsZero() || !j.checkpoint.IsZero() || j.held == "ram" || j.trimFailed || j.container != "" || j.ramGB < minHoldRAM {
			continue
		}
		if best == nil || better(j, best, need, w) {
			best = j
		}
	}
	return best
}

// ramPressure is the RAM ladder. In order, each step only if the one before did not fix it:
//  1. Reclaim: memory nobody needs (the WSL/Docker VM's file cache) is given back first, as soon as
//     RAM is under the reserve or a waiting job lacks only RAM. No job is touched while it runs.
//  2. Suspend: after pressure_seconds (at once when RAM is critical) one job at a time is
//     suspended, and where the OS allows (Windows) its RAM is moved to the pagefile. It resumes
//     when RAM is back. Elsewhere the job is asked to checkpoint and exit instead.
//  3. Stop: only when RAM stays critical, nothing is left to suspend, and the jobs' RAM is what
//     is missing. A job whose RAM would not cover the shortfall is never stopped for it.
func (d *Daemon) ramPressure(now time.Time, floor float64) {
	c := d.cfg
	total, avail := d.snap.RAMTotalGB, d.snap.RAMAvailGB
	if total <= 0 {
		return
	}
	critFloor := total * c.Ladder.KillBelowRAMPct / 100
	short, critical := avail < floor, avail < critFloor
	d.ramCrit = since(d.ramCrit, critical, now)
	if short {
		d.ramLowLast = now
	}
	// Read the VM's own numbers while RAM is tight or a job waits for RAM.
	d.vmWanted = 0
	if c.Reserve.ReclaimVMCache && avail < floor+d.ramMargin()+1 {
		d.vmWanted = 2
	} else if c.Reserve.ReclaimVMCache && d.ramBlocked {
		d.vmWanted = 1
	}

	// What the last suspensions freed, measured a few seconds after the trim.
	for _, j := range d.running() {
		if j.held != "ram" || j.freedAt.IsZero() || now.Sub(j.freedAt) < 3*time.Second {
			continue
		}
		j.freedAt = time.Time{}
		freed := math.Max(0, j.heldRAM-j.ramGB)
		j.freedGB = math.Max(j.freedGB, freed)
		if j.trimFailed = freed < j.heldRAM/2; j.trimFailed {
			logf("%s: suspending it freed only %.1f of its %.1f GB of RAM (locked or shared memory)", j.Name, freed, j.heldRAM)
		} else {
			logf("%s: suspending it freed %.1f GB of RAM (free RAM now %.1f GB)", j.Name, freed, avail)
		}
	}

	// 1. Reclaim.
	wait := false
	if short || d.ramBlocked {
		why := fmt.Sprintf("%.1f GB free, under the human's %.0f%% reserve", avail, d.res.RAMPct)
		if !short {
			why = "jobs are waiting for RAM"
		}
		wait = d.reclaimVM(now, why, short)
		if !wait && critical {
			wait = d.reclaimCritical(now, avail)
		}
	}
	// The VM holds memory but its own numbers aren't in yet (they are read once RAM gets tight):
	// that reading gets 10 s before anything else is tried.
	if short && c.Reserve.ReclaimVMCache && d.vm.HostGB >= reclaimMinGB && !d.vm.Guest && d.reclaim.misses == 0 &&
		now.Sub(d.ramLow) < 10*time.Second && now.Sub(d.reclaim.end) > reclaimEvery {
		wait = true
	}
	// A reclaim gets up to 45 s of critical RAM to show its effect before anything else is tried.
	wait = (wait || now.Sub(d.reclaim.end) < 3*time.Second) && (d.ramCrit.IsZero() || now.Sub(d.ramCrit) < 45*time.Second)

	// 2. Suspend (or, where suspending frees nothing, checkpoint).
	sustained := !d.ramLow.IsZero() && now.Sub(d.ramLow) > dur(c.Ladder.PressureSeconds)
	if !wait && (sustained || critical) && now.Sub(d.lastHold) > holdGap {
		need := floor - avail
		why := d.ramWhy(avail, floor)
		if v := d.ramVictim(need); v != nil && sysinfo.CanTrim {
			d.hold(v, "ram", why, now)
			return
		}
		if sustained && !critical {
			// Nothing can be suspended to any effect: ask the job that covers the shortfall to save
			// and exit (requeued). A job whose RAM would not cover it is left alone.
			held := func(j *job) bool { return j.ramGB >= minHoldRAM && d.ramCovers(j, need) }
			if v := d.victim(held, true, false, need, func(j *job) float64 { return j.ramGB }); v != nil {
				d.requestCheckpoint(v, now, why)
			}
		}
	}

	// 3. Stop: RAM critical, nothing left to suspend, and the jobs hold what is missing.
	if critical && !wait && now.Sub(d.lastKill) > 5*time.Second && now.Sub(d.lastHold) > holdGap {
		need := critFloor - avail
		if sysinfo.CanTrim && d.ramVictim(need) != nil {
			return // still something to suspend first
		}
		var v *job
		for _, j := range d.running() {
			if j.killedAt.IsZero() && (v == nil || better(j, v, need, func(j *job) float64 { return j.ramGB })) {
				v = j
			}
		}
		if v != nil && v.ramGB >= minHoldRAM && d.ramCovers(v, need) {
			v.freedGB, v.oom = math.Max(v.freedGB, v.ramGB), true
			v.reason = fmt.Sprintf("RAM critically low: %.1f GB free (under %.1f GB), %.1f GB short; stopping it frees %.1f GB%s", avail, critFloor, need, v.ramGB, d.vmNote())
			d.killJob(v, now)
		} else if now.Sub(d.critSaid) > time.Minute {
			d.critSaid = now
			logf("RAM critically low (%.1f GB free), but the jobs' RAM is not what is missing: stopping them would not fix it, so none is stopped", avail)
		}
		return
	}

	// Resume, one at a time: RAM has room for what the job takes back, plus the margin, and has had
	// it for 5 s. Each time a job is held again it waits longer (15 s, 30 s, ... 10 min).
	room := avail - floor
	for _, j := range d.running() {
		if j.held != "ram" || !j.checkpoint.IsZero() {
			continue
		}
		back := math.Max(0, j.heldRAM-j.ramGB) + d.ramMargin()
		minHold := time.Duration(math.Min(600, 15*math.Pow(2, float64(j.holds-1)))) * time.Second
		ok := !short && !d.reclaim.on && room >= back && now.Sub(j.heldAt) >= minHold
		j.okSince = since(j.okSince, ok, now)
		if ok && now.Sub(j.okSince) >= 5*time.Second && now.Sub(d.lastHold) > holdGap {
			d.unhold(j, now, fmt.Sprintf("RAM is back (%.1f GB free)", avail))
			return
		}
	}
}

// ramCovers says whether stopping j would fix a RAM shortfall of need: alone, or as the next step
// of stopping every job when together they hold what is missing.
func (d *Daemon) ramCovers(j *job, need float64) bool {
	if j.ramGB >= need {
		return true
	}
	sum := 0.0
	for _, o := range d.running() {
		if o.killedAt.IsZero() {
			sum += o.ramGB
		}
	}
	return sum >= need
}
