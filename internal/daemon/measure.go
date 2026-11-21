package daemon

// Job resource measurements and attribution of outside load.

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

type measured struct {
	members   map[int]int64
	cpuSec    map[int]float64 // per member
	ramGB     float64
	writtenGB float64
}

// ---- measuring ---------------------------------------------------------------------------------

func (d *Daemon) running() []*job {
	var out []*job
	for _, j := range d.jobs {
		if j.State == "running" {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

func (d *Daemon) applyMeasurements(meas map[string]measured, now time.Time, dt time.Duration) {
	c := d.cfg
	capy := d.capacity()
	jobPIDs := map[int]bool{}
	var gpuJobs []*job
	for _, j := range d.running() {
		m, ok := meas[j.ID]
		if !ok {
			continue // started after this tick's scan
		}
		for p := range m.members {
			if _, had := j.pids[p]; !had {
				if err := sysinfo.Background(p); err != nil && p == j.PID {
					logf("%s: background priority: %v", j.Name, err)
				}
			}
			jobPIDs[p] = true
		}
		for p := range j.pids {
			if _, ok := m.members[p]; !ok {
				delete(j.applied, p)
			}
		}
		j.pids = m.members
		// CPU per member: one that exits takes only its own share with it, so the job's use (and
		// with it what counts as the human's) doesn't jump when a worker ends.
		if j.cpuByPID != nil && dt > 0 {
			used := 0.0
			for p, sec := range m.cpuSec {
				if prev, ok := j.cpuByPID[p]; ok {
					used += math.Max(0, sec-prev)
				}
			}
			j.cpuNow = used / dt.Seconds()
		}
		j.cpuByPID, j.ramGB = m.cpuSec, m.ramGB
		if j.written > 0 && dt > 0 {
			rate := math.Max(0, m.writtenGB-j.written) * 1024 / dt.Seconds()
			j.writeRate += (1 - math.Exp(-dt.Seconds()/10)) * (rate - j.writeRate)
		}
		j.written = m.writtenGB
		if !j.measured {
			j.measured, j.written0 = true, m.writtenGB
		}
		if ctr := j.container + j.ctrSeen; ctr != "" { // at most one of the two is set
			for _, o := range d.others {
				if o.ID != "" && strings.HasPrefix(ctr, o.ID) {
					// The container's memory lives in the WSL/Docker VM, outside the job's process
					// tree: it adds to what the job's own processes hold (a driver script, the CLI).
					j.ramGB, j.cpuNow = m.ramGB+o.MemGB, o.CPU/100
				}
			}
		}
		j.vramGB, j.gpuUtil = 0, 0
		for p := range j.pids {
			if g, ok := d.snap.ProcGPU[p]; ok {
				j.vramGB += g.VRAMGB
				j.gpuUtil += g.Util
			}
		}
		if j.GPU {
			gpuJobs = append(gpuJobs, j)
		}
		j.peakRAM = math.Max(j.peakRAM, j.ramGB)
		j.peakVRAM = math.Max(j.peakVRAM, j.vramGB)
	}
	// Jobs the per-process counters can't see (a card without them, or a container's processes,
	// which live outside the job's tree): their VRAM is the card's use beyond the others' usual
	// floor and the jobs that are measured, and their load the card's beyond the measured jobs'.
	// Counted once, as the job's, so it isn't also held against the card as unclaimed need.
	for i, g := range d.snap.GPUs {
		if d.gpuDegraded || i >= len(d.cardFloor) {
			break
		}
		var blind []*job
		seenVRAM, seenUtil := 0.0, 0.0
		for _, j := range gpuJobs {
			if !d.onCard(j, i) {
				continue
			}
			if j.vramGB == 0 && (!g.PerProc || j.container != "") {
				blind = append(blind, j)
			} else {
				seenVRAM += j.vramGB
				seenUtil += j.gpuUtil
			}
		}
		for _, j := range blind {
			j.vramGB = math.Max(0, g.UsedGB-d.cardFloor[i]-seenVRAM) / float64(len(blind))
			if d.snap.ProcUtilKnown {
				j.gpuUtil = math.Max(0, g.Util-seenUtil) / float64(len(blind))
			}
			j.peakVRAM = math.Max(j.peakVRAM, j.vramGB)
		}
	}
	// Without per-process GPU counters (Linux nvidia-smi gives VRAM only), split the card's use.
	// Where counters exist, a job at 0% really is idle: the card's load is someone else's (a game).
	if len(gpuJobs) > 0 && !d.snap.ProcUtilKnown {
		have := false
		for _, j := range gpuJobs {
			have = have || j.gpuUtil > 0
		}
		if !have {
			for _, j := range gpuJobs {
				j.gpuUtil = d.snap.GPUUtil() / float64(len(gpuJobs))
			}
		}
	}
	// Charge machine-minutes (DRF: the dominant share), at the price locked at grant. Idle VRAM is
	// taxed x3, overruns x2. Paused and suspended time is free.
	m := dt.Minutes()
	for _, j := range d.running() {
		if !j.suspended.IsZero() || !j.pausedAt.IsZero() || m <= 0 || m > 0.1 {
			continue
		}
		gpu := 0.0
		if j.GPU {
			gpu = m * math.Max(j.gpuUtil, 10) / 100
		}
		vram := 0.0
		if size := d.cardSize(j, capy.VRAMGB); size > 0 {
			vram = m * j.vramGB / size
			if j.vramGB > 0.5 && j.gpuUtil < 15 {
				vram *= 3 // ESX-style idle tax: holding VRAM without computing
			}
		}
		price := j.price
		if price <= 0 {
			price = policy.Price(c, j.Bid)
		}
		if j.overrun {
			price *= 2
		} else if j.Band == policy.BandSpot && !j.Spot && !j.adopted {
			// Its project ran out of budget mid-run: it is treated as spot from then on (preempted
			// first), so it pays the spot price from then on too.
			price *= policy.SpotPrice
		}
		// The same working time feeds what the command is expected to use next time.
		j.workSec += dt.Seconds()
		j.cpuSec += j.cpuNow * dt.Seconds()
		j.gpuSec += j.gpuUtil * dt.Seconds()
		if j.Net && d.netCap > 0 {
			j.netSum += math.Max(0, d.snap.NetMbps-d.netHuman) * dt.Seconds() // the link's traffic beyond the human's
			j.netSec += dt.Seconds()
		}
		a := d.account(j.Project, now)
		before := a.Charged
		a.Charge(c, gpu, vram, m*j.ramGB/capy.RAMGB, m*j.cpuNow/capy.CPUs, price)
		a.LimitDebt(c, c.Allowance(j.Project))
		j.charged += a.Charged - before
	}
	// What the human and every other program (Docker, games, apps) use: everything not a job.
	jobCPU := 0.0
	for _, j := range d.running() {
		jobCPU += j.cpuNow
	}
	// The least of the last three readings (1.5 s): a job's own start, or one noisy sample, must
	// not grow the human's CPU reserve, which then takes 15 s per CPU to come back.
	d.busyHist[d.busyN%len(d.busyHist)] = math.Max(0, d.snap.CPUBusy-jobCPU)
	d.busyN++
	d.busyUser = d.busyHist[0]
	for _, b := range d.busyHist[:min(d.busyN, len(d.busyHist))] {
		d.busyUser = math.Min(d.busyUser, b)
	}
	d.gpuSeen = d.gpuSeen || len(d.snap.GPUs) > 0
	// The link's size is learned from the most it has carried (forgotten over about a week, so a
	// slower network later is noticed); while no network job runs, its traffic is the human's.
	if dt > 0 {
		if d.netCap == 0 {
			d.netCap = d.st.NetLinkMbps // learned before the last restart
		}
		d.netCap = math.Max(d.snap.NetMbps, d.netCap*math.Exp(-dt.Hours()/168))
		d.st.NetLinkMbps = d.netCap
		netJob := false
		for _, j := range d.running() {
			netJob = netJob || j.Net
		}
		if d.netWasBusy && !netJob {
			d.netQuietFrom = now // the last network job just ended: measure the human from here
		}
		d.netWasBusy = netJob
		if !netJob {
			// ~10 s smoothing: within the 30 s measuring gap it settles to ~95% of the real rate.
			d.netHuman += (1 - math.Exp(-dt.Seconds()/10)) * (d.snap.NetMbps - d.netHuman)
		}
	}
	// The least RAM in use outside jobs lately (the OS, the human's always-open programs): what the
	// machine never frees for a job. Like a card's floor, it drifts up over ~6 h. The WSL/Docker
	// VM is left out: its cache is reclaimed on demand.
	if d.snap.RAMTotalGB > 0 && dt > 0 {
		jobRAM := 0.0
		for _, j := range d.running() {
			jobRAM += j.ramGB
		}
		base := math.Max(0, d.snap.RAMTotalGB-d.snap.RAMAvailGB-jobRAM-d.vm.HostGB)
		if d.ramBase == 0 || base < d.ramBase {
			d.ramBase = base
		} else {
			d.ramBase += (base - d.ramBase) * math.Min(1, dt.Hours()/6)
		}
	}
	// A reading with fewer GPUs than usual is a vendor tool missing a beat, not a card removed:
	// until it lasts a minute, no GPU grants, and the per-card history is kept.
	switch n := len(d.snap.GPUs); {
	case n >= d.gpuMax:
		d.gpuMax, d.gpuShortSince, d.gpuDegraded = n, time.Time{}, false
	case d.gpuShortSince.IsZero():
		d.gpuShortSince, d.gpuDegraded = now, true
	case now.Sub(d.gpuShortSince) > time.Minute:
		d.gpuMax, d.gpuShortSince, d.gpuDegraded = n, time.Time{}, false
		d.cardFloor = nil // the set of cards really changed
	}
	if !d.gpuDegraded && len(d.cardFloor) != len(d.snap.GPUs) {
		d.cardFloor = make([]float64, len(d.snap.GPUs))
		for i, g := range d.snap.GPUs {
			d.cardFloor[i] = g.UsedGB
		}
	}
	for i, g := range d.snap.GPUs {
		if d.gpuDegraded || i >= len(d.cardFloor) {
			break
		}
		jobUse := 0.0
		for _, j := range d.running() {
			if j.GPU && d.onCard(j, i) {
				jobUse += j.vramGB
			}
		}
		base := math.Max(0, g.UsedGB-jobUse) // what's in use that isn't firefighter's
		if base < d.cardFloor[i] {
			d.cardFloor[i] = base
		} else if dt > 0 { // drifts up over ~6 h, so a long-gone low doesn't promise VRAM forever
			d.cardFloor[i] += (base - d.cardFloor[i]) * math.Min(1, dt.Hours()/6)
		}
	}
	d.foreign, d.graphics = map[string]float64{}, map[string]float64{}
	for pid, g := range d.snap.ProcGPU {
		// Where per-process utilization is measured, only real use counts (a browser holding VRAM
		// isn't GPU work). Where it isn't (Linux), holding over 1 GB of VRAM is the only sign.
		// Video encode/decode engines (OBS recording on NVENC, a video call) are separate hardware:
		// they don't slow a CUDA job, so they aren't outside GPU work.
		if jobPIDs[pid] || (d.snap.ProcUtilKnown && g.Compete() < 1) || (!d.snap.ProcUtilKnown && g.VRAMGB < 1) {
			continue
		}
		n := sysinfo.Name(pid)
		if n == "" || contains(c.Reserve.IgnoreGPUApps, n) || d.harvesterProc(n) {
			continue
		}
		if d.containerGPUJob() && (strings.HasPrefix(strings.ToLower(n), "vmmem") || underContainerRuntime(pid)) {
			// A container job's GPU work, seen through the WSL VM (Windows) or under the runtime's
			// shim (Linux): firefighter's own, not an outsider's.
			continue
		}
		u := g.Compete()
		if u < 1 {
			// Unknown per-process load (Linux): holding VRAM isn't computing. Count it busy only
			// while the card itself is busy beyond what firefighter's own jobs explain.
			u = 1
			if d.snap.GPUUtil() >= c.Reserve.ForeignGPUPct && !anyGPUJobRunning(d) {
				u = d.snap.GPUUtil()
			}
		}
		d.foreign[n] = math.Max(d.foreign[n], u)
		if g.Util3D >= 1 {
			d.graphics[n] = math.Max(d.graphics[n], g.Util3D)
		}
	}
	if d.outSince == nil {
		d.outSince = map[string]time.Time{}
	}
	for n := range d.outSince {
		if d.foreign[n] < c.Reserve.ForeignGPUPct {
			delete(d.outSince, n)
		}
	}
	for n, u := range d.foreign {
		if _, ok := d.outSince[n]; !ok && u >= c.Reserve.ForeignGPUPct {
			d.outSince[n] = time.Now()
		}
	}
}
