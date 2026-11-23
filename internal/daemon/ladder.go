package daemon

// Preemption: victim selection, checkpointing, and process control.

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// suspend freezes a job's processes and records them, so a later daemon can thaw them.
func (d *Daemon) suspend(j *job) {
	if d.st.Frozen == nil {
		d.st.Frozen = map[int]int64{}
	}
	for p, created := range j.pids {
		if sysinfo.Same(p, created) {
			d.st.Frozen[p] = created
			_ = sysinfo.Suspend(p)
		}
	}
	d.signalContainer(j, "pause")
	d.saveState() // now, not at the next periodic save: a crash in between would strand them
}

// resume thaws a job's processes.
func (d *Daemon) resume(j *job) {
	for p, created := range j.pids {
		delete(d.st.Frozen, p)
		if sysinfo.Same(p, created) {
			_ = sysinfo.Resume(p)
		}
	}
	d.signalContainer(j, "unpause")
}

// release undoes everything the daemon holds over jobs before it exits: suspended jobs resume and
// pause files go, so a restart or upgrade never leaves a job frozen.
func (d *Daemon) release() {
	for _, j := range d.jobs {
		if !j.suspended.IsZero() {
			d.resume(j)
		}
		_ = os.Remove(PauseFile(j.ID))
	}
	if d.harvest.on {
		stop := d.harvest.stop // the command the running harvester was started with
		if stop == "" {
			stop = d.cfg.Harvester.Stop
		}
		d.harvestDoWait(stop, 2*time.Minute) // behind any start still running, never beside it
	}
}

// ---- preemption ladder ---------------------------------------------------------------------------

func dur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func (d *Daemon) ladder(now time.Time) {
	c := d.cfg

	// Overrun: past the lease x factor without an extension -> spot band, double charge.
	for _, j := range d.running() {
		lease := (j.Minutes + j.extended) * c.Ladder.OverrunFactor
		if j.Minutes > 0 && !j.overrun && !j.adopted && j.leaseUsed(now).Minutes() > lease {
			j.overrun, j.Band = true, policy.BandSpot
			logf("%s: past its %.0f-minute lease; now spot (charged double, preempted first)", j.Name, j.Minutes+j.extended)
		}
	}

	// Memory pressure, measured first: the game ladder below only evicts suspended jobs when the
	// memory they hold is what the human is short of.
	ramFloor := d.ramFloor()
	ramShort := d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < ramFloor
	vramCard := d.vramShortCard()
	d.ramLow = since(d.ramLow, ramShort, now)
	d.vramLow = since(d.vramLow, vramCard >= 0, now)

	// The human's state holds the GPU (game by default): cooperative pause, then suspend. A job
	// stays suspended while the human needs the GPU; it is only evicted if the memory it holds is
	// what's short (and --evict never jobs never are, short of critical RAM).
	gpuHold, netHold := d.gpuHeld(), d.user == "game" && c.Network.PauseInGame
	for _, j := range d.running() {
		if (!j.GPU && !j.Net) || !j.killedAt.IsZero() || j.held != "" {
			continue // a job held for RAM or disk is the pressure rules' until they let it go
		}
		hold := (j.GPU && gpuHold) || (j.Net && netHold)
		acked := exists(PausedFile(j.ID))
		switch {
		case hold && j.pausedAt.IsZero() && j.checkpoint.IsZero() && j.Net && !j.GPU:
			// A download can't be told to pause and checked (no per-process network numbers):
			// suspend it outright; it resumes after the game.
			j.pausedAt, j.reason = now, "the human is gaming (network latency)"
			j.suspended = now
			d.suspend(j)
			logf("%s: suspended: %s", j.Name, j.reason)
		case hold && j.pausedAt.IsZero() && j.checkpoint.IsZero():
			j.pausedAt, j.reason = now, "the human needs the GPU ("+d.why()+")"
			touch(PauseFile(j.ID))
			logf("%s: pause requested: %s", j.Name, j.reason)
		case hold && !j.pausedAt.IsZero() && j.suspended.IsZero() && j.checkpoint.IsZero() && !acked &&
			now.Sub(j.pausedAt) > dur(c.Ladder.PauseAckSeconds):
			j.suspended = now
			d.suspend(j)
			logf("%s: no pause ack in %.0fs, suspended", j.Name, c.Ladder.PauseAckSeconds)
		case hold && !j.pausedAt.IsZero() && j.suspended.IsZero() && j.checkpoint.IsZero() && acked:
			// An ack is a promise, not proof: a job that acked but keeps computing is suspended.
			// Only measurable with per-process counters; elsewhere the job's share is a guess.
			j.ackBusy = since(j.ackBusy, d.snap.ProcUtilKnown && j.gpuUtil >= 10, now)
			if !j.ackBusy.IsZero() && now.Sub(j.ackBusy) > dur(c.Ladder.PauseAckSeconds) {
				j.suspended = now
				d.suspend(j)
				logf("%s: acked the pause but kept the GPU at %.0f%%; suspended", j.Name, j.gpuUtil)
			}
		case hold && !j.suspended.IsZero() && j.checkpoint.IsZero() && j.GPU &&
			now.Sub(j.suspended) > dur(c.Ladder.SuspendMaxSeconds) &&
			((j.Evict != "never" && ramShort && j.ramGB > 0.5 && (!sysinfo.CanTrim || j.trimFailed)) || (vramCard >= 0 && d.onCard(j, vramCard))):
			// evict=never doesn't keep VRAM the human is short of through a whole game (review round 6).
			// A GPU job suspended past the limit and holding memory the human is short of. It ignored
			// the pause file, so it can't checkpoint: resuming it would only let it compute through
			// the game. Stop it (requeued). A download frozen for the game is left to the RAM
			// pressure rule, which picks the job whose RAM covers the shortfall and thaws it to save.
			// Where a suspended job's RAM can be moved out (Windows), the RAM rule does that instead.
			j.reason = "suspended past the limit, holding memory the human needs"
			d.killJob(j, now)
		case !hold && !j.pausedAt.IsZero():
			d.unpause(j, now)
			logf("%s: resumed", j.Name)
		}
	}

	// RAM and disk: reclaim what nobody needs, then suspend, and stop a job only as a last resort.
	d.ramPressure(now, ramFloor)
	d.diskPressure(now)

	// VRAM pressure: under the reserve for PressureSeconds -> checkpoint one victim, the one whose
	// leaving actually covers the shortfall.
	if !d.vramLow.IsZero() && now.Sub(d.vramLow) > dur(c.Ladder.PressureSeconds) {
		card := vramCard
		short := d.res.VRAMGB
		if card >= 0 && card < len(d.snap.GPUs) {
			g := d.snap.GPUs[card]
			short = math.Max(0.1, d.res.VRAMGB-(g.VRAMGB-g.UsedGB)) // what the human is missing, not the whole reserve
		}
		// Still short after three pressure periods: critical, so an evict=never job is asked too
		// (never is not an exemption from the human's reserve; review round 6).
		critical := now.Sub(d.vramLow) > 3*dur(c.Ladder.PressureSeconds)
		if v := d.victim(func(j *job) bool { return j.GPU && d.onCard(j, card) }, true, critical, short, func(j *job) float64 { return j.vramGB }); v != nil {
			why := fmt.Sprintf("VRAM below the human's %.1f GB reserve", d.res.VRAMGB)
			if capGB, total := d.vramCap(v); capGB > 0 && v.vramGB > capGB {
				// Say how to stay under it: a framework's cache grows to what the card has, and
				// the job is stopped again each run until it caps itself (fdf58d99).
				why += fmt.Sprintf("; it holds %.1f GB and may hold %.1f GB here: cap it, e.g. torch.cuda.set_per_process_memory_fraction(float(os.environ['FIREFIGHTER_VRAM_FRACTION'])) (%.2f of the %.1f GB card)",
					v.vramGB, capGB, capGB/total, total)
			}
			d.requestCheckpoint(v, now, why)
		}
	}

	// A spot GPU job yields to the head of the in-budget GPU line only when that helps: the slots
	// are full, the spot job has had its minimum runtime, and its VRAM is what the head lacks. One
	// at a time: a spot job already leaving frees what the head needs.
	leaving := false
	for _, j := range d.running() {
		if j.GPU && !j.checkpoint.IsZero() && j.killedAt.IsZero() {
			leaving = true
		}
	}
	if !gpuHold && !leaving {
		ram, vram := d.headroom(now)
		var head *policy.Ticket
		for _, t := range d.waiting() {
			// The head as admission sees it: one that can start once there's room (not backing
			// off, not waiting for the human to leave).
			if t.GPU && t.Band < policy.BandSpot && !d.backingOff(t.ID, now) && !d.waitsForAway(t) {
				head = t
				break
			}
		}
		card := -1
		if head != nil {
			card, _ = d.cardFor(head)
		}
		slotsFull := d.gpuBusySlots() >= c.GPU.MaxJobs
		if card >= 0 {
			slotsFull = !d.cardOpen(card)
		}
		if head != nil && head.NeedRAM <= ram && (card >= 0 || len(d.snap.GPUs) == 0) {
			// Only a spot job on the card the head would use can make room for it, and the head's
			// need there is the clamped one.
			need, left := d.needOn(head, card), vram
			if card >= 0 {
				left = d.cardLeft(head)
			}
			fits := need <= left || len(d.snap.GPUs) == 0 // no discrete GPU: VRAM is RAM, admitted as RAM
			// When the head fits its card and only a slot is missing, a spot job on any card frees
			// one; when VRAM on its card is the problem, only a spot job on that card helps.
			var spot *job
			for _, j := range d.running() {
				if j.GPU && j.Band == policy.BandSpot && j.checkpoint.IsZero() && j.Evict != "never" && (fits || card < 0 || d.onCard(j, card)) &&
					j.leaseUsed(now).Minutes() >= c.Ladder.MinRuntimeMinutes && (spot == nil || j.vramGB > spot.vramGB) {
					spot = j
				}
			}
			if spot != nil && (slotsFull || !fits) && (fits || need <= left+spot.vramGB+0.5) {
				d.requestCheckpoint(spot, now, "spot job yields to "+head.Name)
			}
		}
	}

	// Checkpoint grace over: SIGTERM where there is one (10 s to exit), then kill, once.
	for _, j := range d.running() {
		if j.checkpoint.IsZero() || !j.killedAt.IsZero() || now.Sub(j.checkpoint) <= dur(c.Ladder.GraceSeconds) {
			continue
		}
		if runtime.GOOS != "windows" && j.termAt.IsZero() {
			j.termAt = now
			d.each(j, sysinfo.Terminate)
			logf("%s: grace over, SIGTERM", j.Name)
		} else if runtime.GOOS == "windows" || now.Sub(j.termAt) > 10*time.Second {
			d.killJob(j, now)
		}
	}
}

func (d *Daemon) why() string {
	if d.user == "game" {
		return d.gameWhy
	}
	return d.user
}

func (d *Daemon) unpause(j *job, now time.Time) {
	_ = os.Remove(PauseFile(j.ID))
	if !j.suspended.IsZero() {
		d.resume(j)
	}
	if !j.pausedAt.IsZero() {
		j.pausedTotal += now.Sub(j.pausedAt)
	}
	if j.held != "" {
		j.heldTotal += now.Sub(j.heldAt)
	}
	j.pausedAt, j.suspended, j.ackBusy, j.reason = time.Time{}, time.Time{}, time.Time{}, ""
	j.held, j.heldDrive, j.okSince, j.freedAt = "", "", time.Time{}, time.Time{}
}

func since(t time.Time, cond bool, now time.Time) time.Time {
	if !cond {
		return time.Time{}
	}
	if t.IsZero() {
		return now
	}
	return t
}

// victim picks the job to evict among those keep() accepts: never-evict jobs only when critical,
// then jobs whose weight covers the shortfall `need`, then the highest band, then the heaviest.
// With oneAtATime, nothing is picked while another eviction is still in its grace period.
func (d *Daemon) victim(keep func(*job) bool, oneAtATime, critical bool, need float64, weight func(*job) float64) *job {
	var best *job
	for _, j := range d.running() {
		if !j.killedAt.IsZero() {
			continue
		}
		if !j.checkpoint.IsZero() {
			if oneAtATime {
				return nil
			}
			if !critical {
				continue
			}
		}
		if (keep != nil && !keep(j)) || (j.Evict == "never" && !critical) {
			continue
		}
		if best == nil || better(j, best, need, weight) {
			best = j
		}
	}
	return best
}

func better(a, b *job, need float64, w func(*job) float64) bool {
	if (a.Evict == "never") != (b.Evict == "never") {
		return b.Evict == "never"
	}
	if ca, cb := w(a) >= need, w(b) >= need; ca != cb {
		return ca // leaving actually fixes the shortfall
	}
	if a.Band != b.Band {
		return a.Band > b.Band
	}
	return w(a) > w(b)
}

// requestCheckpoint asks a job to save and exit; a suspended job is resumed first so it can.
func (d *Daemon) requestCheckpoint(j *job, now time.Time, why string) {
	if !j.checkpoint.IsZero() || !j.killedAt.IsZero() {
		return
	}
	if !j.suspended.IsZero() && (j.held != "" || (j.Net && !j.GPU)) {
		// A download frozen for a game, or a job held for RAM or disk (neither was asked to pause):
		// thaw it so it can save and exit in its grace period like any job; a few seconds of
		// running beat losing the work.
		d.resume(j)
		if j.held != "" {
			j.heldTotal += now.Sub(j.heldAt)
		}
		j.suspended, j.held, j.heldDrive = time.Time{}, "", ""
	} else if !j.suspended.IsZero() {
		// Suspended means it ignored the pause file, so it won't read the checkpoint file either:
		// resuming it would only let it compute (through the human's game) until the grace ends.
		j.reason = why
		d.killJob(j, now)
		return
	}
	// A paused job waits in its pause loop: lift the pause, so it sees the checkpoint request.
	_ = os.Remove(PauseFile(j.ID))
	if !j.pausedAt.IsZero() {
		j.pausedTotal += now.Sub(j.pausedAt)
		j.pausedAt = time.Time{}
	}
	j.checkpoint, j.reason = now, why
	touch(CheckpointFile(j.ID))
	if j.Evict == "force" {
		d.killJob(j, now)
		return
	}
	logf("%s: checkpoint and exit requested (%s); %.0fs grace", j.Name, why, d.cfg.Ladder.GraceSeconds)
	d.broadcast()
}

func (d *Daemon) killJob(j *job, now time.Time) {
	if !j.killedAt.IsZero() {
		return
	}
	j.killedAt = now
	if j.held != "" {
		j.heldTotal += now.Sub(j.heldAt)
		j.held = ""
	}
	if j.checkpoint.IsZero() {
		j.checkpoint = now
	}
	logf("%s: killed (%s)", j.Name, j.reason)
	if !j.suspended.IsZero() {
		d.resume(j) // a stopped process can't die cleanly on some systems (and a paused container is unpaused first)
	}
	if j.container != "" {
		d.docker(j.engine, "kill", j.container)
	}
	d.each(j, sysinfo.Kill)
	d.lastKill = now
}

// each applies f to every member still the same process it was when it joined the job.
func (d *Daemon) each(j *job, f func(int) error) {
	for p, created := range j.pids {
		if sysinfo.Same(p, created) {
			_ = f(p)
		}
	}
}

// reap closes tickets whose client is gone. A running job whose client died (killed agent,
// closed terminal) is stopped too, so it can't hold the GPU with nobody waiting for it.
func (d *Daemon) reap(now time.Time) {
	for id, j := range d.jobs {
		if j.State == "done" {
			continue
		}
		clientGone := j.ClientPID > 1 && (!sysinfo.Alive(j.ClientPID) || (j.clientAt != 0 && !sysinfo.Same(j.ClientPID, j.clientAt)))
		jobGone := j.PID <= 1 || !sysinfo.Same(j.PID, j.rootCreated)
		switch {
		case j.State == "granted" && clientGone:
			d.finish(id, -1, "lost", "")
		case j.State == "waiting" && clientGone:
			d.finish(id, -1, "cancelled", "")
		case j.State == "running" && j.adopted && jobGone:
			outcome := "ended"
			if !j.killedAt.IsZero() {
				outcome = "killed" // the guard stopped it: its receipt says why
			}
			d.finish(id, -1, outcome, "")
		case j.State == "running" && !j.adopted && clientGone:
			if !jobGone && j.killedAt.IsZero() {
				j.reason = "its firefighter client exited"
				d.killJob(j, now)
			}
			d.finish(id, -1, "lost", "")
		case j.State == "granted" && now.Sub(j.granted) > 2*time.Minute:
			j.State = "waiting"
			logf("%s: granted 2 min ago but never started; back to waiting", j.Name)
		}
	}
}

// refreshBands re-places tickets as budgets drain and refill. A running job whose project's
// budget runs out drops to the spot band (preempted first) until the refill.
func (d *Daemon) refreshBands(now time.Time) {
	for _, j := range d.jobs {
		if j.Spot || j.adopted || j.State == "done" || j.State == "granted" {
			continue
		}
		a := d.account(j.Project, now)
		inBudget := a.Left(d.cfg, d.cfg.Allowance(j.Project)) > 0
		switch {
		case j.State == "waiting":
			j.Band = policy.BandFor(j.Ticket, inBudget)
		case j.State == "running" && !inBudget && j.Band != policy.BandSpot:
			j.Band = policy.BandSpot
			logf("%s: %s is out of budget; job now spot (preempted first)", j.Name, j.Project)
		case j.State == "running" && inBudget && j.Band == policy.BandSpot && !j.overrun:
			j.Band = policy.BandFor(j.Ticket, true)
		}
	}
}
