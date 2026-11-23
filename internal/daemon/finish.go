package daemon

// Job completion, receipts, and learning from finished runs.

import (
	"math"
	"os"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// finish closes a ticket: learns runtime and peaks, writes the receipt.
func (d *Daemon) finish(id string, exit int, outcome, log string) {
	j := d.jobs[id]
	if j == nil || j.State == "done" {
		return
	}
	now := time.Now()
	ran := 0.0
	if !j.Started.IsZero() {
		ran = now.Sub(j.Started).Minutes()
	}
	// Learn from any run that was measured. Not "still has pids": the process scan can drop an exited
	// job's pids before its client reports done, and the run was then thrown away (CI, 11/20).
	if j.State == "running" && outcome != "lost" && (j.measured || len(j.pids) > 0) {
		h := d.st.History[histKey(j.Ticket)]
		if h == nil {
			h = &History{}
			d.st.History[histKey(j.Ticket)] = h
		}
		worked := j.leaseUsed(now).Minutes()                   // paused, suspended and asleep time isn't runtime
		if j.Minutes > 0 && outcome == "ok" && worked >= 0.5 { // sub-30 s runs say nothing about runtime
			h.Ratios = append(h.Ratios, math.Max(0.05, worked/j.Minutes))
			if len(h.Ratios) > 5 {
				h.Ratios = h.Ratios[len(h.Ratios)-5:]
			}
		}
		h.PeakVRAM = math.Max(h.PeakVRAM*0.9, j.peakVRAM) // slowly forgets old peaks
		h.PeakRAM = math.Max(h.PeakRAM*0.9, j.peakRAM)
		h.Runs++
		h.Last = now
		d.learnRun(h, j, now)
	}
	if !j.suspended.IsZero() {
		d.resume(j)
	}
	// Workers that outlive their job (data loaders holding VRAM) would make the next GPU job run
	// out of memory: a finished job's leftovers go with it. Adopted processes are left alone, and so
	// are shared build servers (guard.keep_alive) the human's own builds reuse.
	if !j.adopted && j.State == "running" {
		for p, created := range j.pids {
			if p != j.PID && sysinfo.Same(p, created) && !KeepAlive(d.cfg.Guard.KeepAlive, sysinfo.Name(p)) {
				_ = sysinfo.Kill(p)
			}
		}
		// A container started by `docker run` outlives its CLI when the client is interrupted.
		if j.container != "" && outcome != "ok" && outcome != "failed" {
			d.docker(j.engine, "kill", j.container)
		}
		// One only guessed to be the job's (timing) isn't stopped, but it is never counted as the
		// human's load either: status shows it as this job's leftover.
		if j.ctrSeen != "" {
			if d.leftover == nil {
				d.leftover = map[string]string{}
			}
			for _, c := range d.others {
				if strings.HasPrefix(j.ctrSeen, c.ID) {
					d.leftover[c.ID] = j.Name
				}
			}
		}
	}
	for _, f := range []string{PauseFile(id), PausedFile(id), CheckpointFile(id)} {
		_ = os.Remove(f)
	}
	waited := now.Sub(j.Created).Minutes() - ran
	if j.held != "" {
		j.heldTotal += now.Sub(j.heldAt)
	}
	if j.oom && j.failure == nil && j.charged > 0 { // not when it ran itself out of memory
		// The guard stopped it for the human's RAM and its work is lost: it isn't billed as well.
		d.account(j.Project, now).Refund(d.cfg, j.charged)
		logf("%s: its %.1f machine-minutes are refunded (stopped by the guard for RAM)", j.Name, j.charged)
		j.charged = 0
	}
	rec := &Receipt{ID: id, Project: j.Project, Name: j.Name, Exit: exit, Outcome: outcome, Minutes: round1(ran),
		Declared: j.Minutes, Waited: round1(waited), Charged: round1(j.charged), PeakVRAM: round1(j.peakVRAM),
		PeakRAM: round1(j.peakRAM), Log: log, Ended: now, Requeue: outcome == "evicted",
		Client: j.ClientPID, ClientAt: j.clientAt,
		Held: j.holds, HeldMinutes: round1(j.heldTotal.Minutes()), FreedRAM: round1(j.freedGB), Failure: j.failure}
	if outcome == "evicted" || outcome == "killed" || outcome == "lost" || !j.killedAt.IsZero() {
		rec.Reason = j.reason
	}
	j.receipt = rec
	if d.backoff == nil {
		d.backoff = map[string]backoff{}
	}
	if j.Quick && (j.container != "" || strings.Contains(strings.ToLower(j.Command), "docker")) && d.cfg.Reserve.ReclaimVMCache {
		// A quick look into an image (docker run --rm img ls) reads it into the WSL VM's file cache,
		// GBs no one needs afterwards (11/20: three such reads outside firefighter each held GBs).
		logf("%s: quick container job done; dropping the WSL/Docker VM's file cache", j.Name)
		name := d.vm.Name
		go func() {
			if err := sysinfo.ReclaimVM(name); err != nil {
				logf("RAM: the VM's cache could not be dropped: %v", err)
			}
		}()
	}
	if outcome == "evicted" {
		d.backoff[id] = backoff{n: d.backoff[id].n + 1, at: now, why: j.reason}
	} else {
		delete(d.backoff, id)
	}
	if j.Net && j.State == "running" {
		last := true
		for _, o := range d.jobs {
			last = last && (o == j || !o.Net || o.State != "running")
		}
		if last {
			d.netQuietFrom, d.netWasBusy = now, false // measure the human before the next download, starting now
		}
	}
	j.State = "done"
	d.st.Receipts[id] = rec
	logf("%s: %s (exit %d) after %.1f min, waited %.1f, charged %.1f", j.Name, outcome, exit, ran, waited, j.charged)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// learnRun adds one finished run to its command's usage histograms: the peaks of what it must not
// run out of (RAM, VRAM, disk written) and the means of what it can be throttled on (CPUs, GPU
// load, network). A run stopped for lack of RAM records more than it reached (usage.OOMBump): its
// real need is above that. Rates need 10 s of work to mean anything.
func (d *Daemon) learnRun(h *History, j *job, now time.Time) {
	add := func(hp **usage.Hist, first, v, half float64) {
		if *hp == nil {
			*hp = usage.NewHist(first)
		}
		usage.AddRun(*hp, v, half, now)
	}
	h.Name, h.GPUJob = j.Name, j.GPU
	ram := j.peakRAM
	if j.oom {
		ram = usage.OOMBump(ram)
	}
	add(&h.RAM, 0.05, ram, usage.RunHalfLifePeak)
	if j.GPU {
		vram := j.peakVRAM
		// Out of GPU memory: its need is above what it reached. Only from the measured peak, and only
		// when that peak was near what it was admitted with (a log line alone, or an OOM the job
		// recovered from, raises nothing), and never past the card (review 11/20).
		if j.vramOOM && j.NeedVRAM > 0 && j.peakVRAM >= 0.8*j.NeedVRAM {
			vram = usage.OOMBump(j.peakVRAM)
			if t, _ := d.snap.VRAMTotal(); t > 0 {
				vram = math.Min(vram, t)
			}
		}
		add(&h.VRAM, 0.05, vram, usage.RunHalfLifePeak)
	}
	if j.workSec < 10 {
		return
	}
	add(&h.CPU, 0.05, j.cpuSec/j.workSec, usage.RunHalfLifeRate)
	add(&h.Disk, 0.01, math.Max(0, j.written-j.written0), usage.RunHalfLifePeak)
	add(&h.Mins, 0.1, j.workSec/60, usage.RunHalfLifeRate)
	if j.GPU {
		add(&h.GPU, 1, j.gpuSec/j.workSec, usage.RunHalfLifeRate)
	}
	if j.Net && j.netSec > 0 {
		add(&h.Net, 0.1, j.netSum/j.netSec, usage.RunHalfLifeRate)
	}
}

// KeepAlive says whether a process name is a shared build server (guard.keep_alive) that outlives
// the job that started it.
func KeepAlive(names []string, name string) bool {
	n := strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".exe"))
	for _, k := range names {
		if k != "" && strings.Contains(n, strings.ToLower(k)) {
			return true
		}
	}
	return false
}
