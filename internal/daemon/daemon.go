// Package daemon is the guard: one per user per machine. It samples the machine twice a second,
// sizes the human's reserve, admits waiting jobs in policy order, charges budgets, and walks the
// preemption ladder when the human needs resources back.
package daemon

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

const tick = 500 * time.Millisecond

type job struct {
	*policy.Ticket
	rootCreated int64
	pids        map[int]int64  // member pid -> start time (pid + start time = one process, for good)
	applied     map[int]string // pid -> cpu set applied
	cpuPrev     float64
	cpuNow      float64 // CPUs in use
	ramGB       float64
	vramGB      float64
	gpuUtil     float64
	peakVRAM    float64
	peakRAM     float64
	charged     float64
	overrun     bool
	granted     time.Time
	pausedAt    time.Time // cooperative pause flag set
	pausedTotal time.Duration
	suspended   time.Time
	checkpoint  time.Time // checkpoint-and-exit requested
	termAt      time.Time
	killedAt    time.Time
	reason      string
	container   string
	receipt     *Receipt
	extended    float64
	price       float64 // locked at grant: bid price x congestion x surcharge
	adopted     bool    // auto-adopted: heavy work an agent started outside firefighter
	backfilled  bool    // started ahead of a GPU head waiting on VRAM: can't extend its lease
}

func newJob(t *policy.Ticket) *job {
	j := &job{Ticket: t, pids: map[int]int64{}, applied: map[int]string{}}
	if t.PID > 1 {
		j.rootCreated = sysinfo.Created(t.PID)
		if j.rootCreated != 0 {
			j.pids[t.PID] = j.rootCreated
		}
	}
	return j
}

type Daemon struct {
	mu       sync.Mutex
	cfg      config.Config
	cfgMod   time.Time
	cfgErr   string
	st       saved
	jobs     map[string]*job
	reader   *sysinfo.Reader
	snap     sysinfo.Snapshot
	res      policy.ReserveCtl
	user     string
	gameWhy  string
	gameTill time.Time
	busyUser float64
	utilEWMA float64 // GPU utilization smoothed over ~30 s: packing decisions ignore momentary dips
	ramLow   time.Time
	vramLow  time.Time
	names    map[string]bool
	namesAt  time.Time
	foreign  map[string]float64 // non-job GPU users -> %
	graphics map[string]float64 // non-job 3D-engine users -> % (games, video)
	others   []sysinfo.Container
	notify   chan struct{}
	rng      *rand.Rand
	harvest  struct {
		on        bool
		quietFrom time.Time
	}
	// owned by the Run goroutine only
	scanner   *sysinfo.Scanner
	rogueAt   time.Time
	rogueSeen map[int]time.Time
	dockerAt  time.Time
	docking   atomic.Bool

	saveAt   time.Time
	statusAt time.Time
	started  time.Time
	Version  string
	Token    string
	Port     int
	stop     chan struct{}
}

func New(version string) *Daemon {
	d := &Daemon{
		jobs:    map[string]*job{},
		reader:  sysinfo.NewReader(),
		st:      load(),
		notify:  make(chan struct{}),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		started: time.Now(),
		user:    "present",
		Version: version,
		stop:    make(chan struct{}),
	}
	d.reloadConfig()
	_ = os.MkdirAll(signalDir(), 0o755)
	// Pause files left by a daemon that died mid-game would freeze cooperative jobs: clear them.
	if ents, err := os.ReadDir(signalDir()); err == nil {
		for _, e := range ents {
			_ = os.Remove(filepath.Join(signalDir(), e.Name()))
		}
	}
	// The harvester's state isn't known after a restart: stop it; it restarts after the idle wait.
	go shell(d.cfg.Harvester.Stop)
	return d
}

// release undoes everything the daemon holds over jobs before it exits: suspended jobs resume and
// pause files go, so a restart or upgrade never leaves a job frozen.
func (d *Daemon) release() {
	for _, j := range d.jobs {
		if !j.suspended.IsZero() {
			d.each(j, sysinfo.Resume)
			d.signalContainer(j, "unpause")
		}
		_ = os.Remove(PauseFile(j.ID))
	}
	if d.harvest.on {
		shell(d.cfg.Harvester.Stop)
	}
}

func signalDir() string { return filepath.Join(config.Home(), "signals") }

// Signal files a job may watch (also passed to it as environment variables).
func PauseFile(id string) string      { return filepath.Join(signalDir(), id+".pause") }
func PausedFile(id string) string     { return filepath.Join(signalDir(), id+".paused") }
func CheckpointFile(id string) string { return filepath.Join(signalDir(), id+".checkpoint") }

func (d *Daemon) reloadConfig() {
	fi, err := os.Stat(config.Path())
	var mod time.Time
	if err == nil {
		mod = fi.ModTime()
	}
	if !d.cfgMod.IsZero() && mod.Equal(d.cfgMod) {
		return
	}
	d.cfgMod = mod
	c, err := config.Load()
	if err != nil {
		d.cfgErr = err.Error()
		logf("config: %v (keeping the last good settings)", err)
		if d.cfg.Order.Policy == "" {
			d.cfg = config.Defaults()
		}
		return
	}
	d.cfgErr = ""
	d.cfg = c
}

// broadcast wakes every long-poll waiter.
func (d *Daemon) broadcast() {
	close(d.notify)
	d.notify = make(chan struct{})
}

// ---- the loop: slow probes outside the lock, decisions inside ---------------------------------------

type root struct {
	id        string
	pid       int
	created   int64
	known     map[int]int64
	container string
}

type measured struct {
	members map[int]int64
	cpuSec  float64
	ramGB   float64
}

// Run is the control loop. It never returns.
func (d *Daemon) Run() {
	t := time.NewTicker(tick)
	defer t.Stop()
	last := time.Now()
	for {
		var now time.Time
		select {
		case now = <-t.C:
		case <-d.stop:
			return
		}
		snap := d.reader.Read()

		d.mu.Lock()
		var roots []root
		for _, j := range d.jobs {
			if j.State == "running" {
				known := make(map[int]int64, len(j.pids))
				for p, c := range j.pids {
					known[p] = c
				}
				roots = append(roots, root{j.ID, j.PID, j.rootCreated, known, j.container})
			}
		}
		cfg := d.cfg
		d.mu.Unlock()

		meas := map[string]measured{}
		governed := map[int]bool{os.Getpid(): true}
		for _, r := range roots {
			m := sysinfo.Members(r.pid, r.created, r.known)
			pids := make([]int, 0, len(m))
			for p := range m {
				pids = append(pids, p)
				governed[p] = true
			}
			cpu, ram := sysinfo.Usage(pids)
			meas[r.id] = measured{m, cpu, ram}
		}
		rogues := d.findRogues(now, cfg, snap, governed)
		d.refreshDocker(now)

		d.mu.Lock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					logf("loop panic: %v", r)
				}
			}()
			d.step(snap, meas, rogues, now, now.Sub(last))
		}()
		d.mu.Unlock()
		last = now
	}
}

// refreshDocker reads `docker stats` in the background every 15 s; a hung Docker never stalls the loop.
func (d *Daemon) refreshDocker(now time.Time) {
	if d.docking.Load() || now.Sub(d.dockerAt) < 15*time.Second {
		return
	}
	d.docking.Store(true)
	d.dockerAt = now
	go func() {
		cs := sysinfo.Containers()
		d.mu.Lock()
		d.others = cs
		d.mu.Unlock()
		d.docking.Store(false)
	}()
}

func (d *Daemon) step(snap sysinfo.Snapshot, meas map[string]measured, rogues []rogue, now time.Time, dt time.Duration) {
	d.reloadConfig()
	c := d.cfg
	d.snap = snap
	if dt > 0 {
		d.utilEWMA += (1 - math.Exp(-dt.Seconds()/30)) * (snap.GPUUtil() - d.utilEWMA)
	} else {
		d.utilEWMA = snap.GPUUtil()
	}
	d.applyMeasurements(meas, now, dt)
	d.observeOccupancy(now, dt)
	d.senseUser(now)
	d.adoptRogues(rogues, now)
	d.res.Update(c, d.user, d.busyUser, now)
	d.enforceCPU()
	d.ladder(now)
	d.reap(now)
	d.refreshBands(now)
	d.admit(now)
	d.harvester(now)
	if now.Sub(d.saveAt) > 10*time.Second {
		d.saveAt = now
		d.prune(now)
		if err := d.st.write(); err != nil {
			logf("save: %v", err)
		}
	}
	if now.Sub(d.statusAt) > 15*time.Second {
		d.statusAt = now
		d.writeStatus()
	}
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
		if j.cpuPrev > 0 && dt > 0 {
			j.cpuNow = math.Max(0, (m.cpuSec-j.cpuPrev)/dt.Seconds())
		}
		j.cpuPrev, j.ramGB = m.cpuSec, m.ramGB
		if j.container != "" {
			for _, o := range d.others {
				if o.Name == j.container {
					j.ramGB, j.cpuNow = o.MemGB, o.CPU/100
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
	// Without per-process GPU counters (Linux nvidia-smi gives VRAM only), split the card's use.
	if len(gpuJobs) > 0 {
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
		if capy.VRAMGB > 0 {
			vram = m * j.vramGB / capy.VRAMGB
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
		}
		a := d.account(j.Project, now)
		before := a.Charged
		a.Charge(c, gpu, vram, m*j.ramGB/capy.RAMGB, m*j.cpuNow/capy.CPUs, price)
		j.charged += a.Charged - before
	}
	// What the human and every other program (Docker, games, apps) use: everything not a job.
	jobCPU := 0.0
	for _, j := range d.running() {
		jobCPU += j.cpuNow
	}
	d.busyUser = math.Max(0, d.snap.CPUBusy-jobCPU)
	d.foreign, d.graphics = map[string]float64{}, map[string]float64{}
	for pid, g := range d.snap.ProcGPU {
		// Where per-process utilization is measured, only real use counts (a browser holding VRAM
		// isn't GPU work). Where it isn't (Linux), holding over 1 GB of VRAM is the only sign.
		if jobPIDs[pid] || (d.snap.ProcUtilKnown && g.Util < 1) || (!d.snap.ProcUtilKnown && g.VRAMGB < 1) {
			continue
		}
		n := sysinfo.Name(pid)
		if n == "" || contains(c.Reserve.IgnoreGPUApps, n) {
			continue
		}
		u := g.Util
		if u < 1 {
			u = c.Reserve.ForeignGPUPct // unknown utilization, known VRAM: assume busy
		}
		d.foreign[n] = math.Max(d.foreign[n], u)
		if g.Util3D >= 1 {
			d.graphics[n] = math.Max(d.graphics[n], g.Util3D)
		}
	}
}

func (d *Daemon) capacity() policy.Capacity {
	_, vt := d.snap.MaxFreeVRAM()
	return policy.Capacity{CPUs: float64(max(1, d.snap.CPUs)), RAMGB: math.Max(1, d.snap.RAMTotalGB), VRAMGB: vt}
}

func (d *Daemon) account(project string, now time.Time) *policy.Account {
	a := d.st.Accounts[project]
	if a == nil {
		na := d.newAccount(project, now)
		a = &na
		d.st.Accounts[project] = a
	}
	a.Decay(now, d.cfg.Order.HalfLifeHours)
	a.Refill(now, d.cfg, d.cfg.Allowance(project))
	return a
}

// ---- the human ---------------------------------------------------------------------------------

func (d *Daemon) senseUser(now time.Time) {
	c := d.cfg
	if now.Sub(d.namesAt) > 5*time.Second {
		d.names, d.namesAt = sysinfo.Names(), now
	}
	why := ""
	if d.snap.Foreground != "" {
		why = "fullscreen " + d.snap.Foreground
	}
	for _, a := range c.Reserve.ExclusiveApps {
		if d.names[strings.ToLower(a)] || d.names[strings.ToLower(sysinfo.ExeName(a))] {
			why = a
		}
	}
	// Heavy 3D-engine use by the app in front is play (a game in a window); 3D use by background
	// processes, and CUDA compute anywhere outside firefighter, is load, not the human.
	for n, u := range d.graphics {
		if u >= c.Reserve.ForeignGPUPct && (d.snap.ForegroundApp == "" || strings.EqualFold(n, d.snap.ForegroundApp)) && d.snap.ProcUtilKnown {
			why = fmt.Sprintf("%s using %.0f%% of the 3D engine", n, u)
		}
	}
	if why != "" {
		d.gameWhy, d.gameTill = why, now.Add(time.Duration(c.Reserve.GameCoolSeconds*float64(time.Second)))
	}
	game := now.Before(d.gameTill)
	idle := d.snap.Idle
	if idle < 0 {
		idle = (c.Reserve.ActiveSeconds + c.Reserve.AwaySeconds) / 2 // unknown (headless): present
	}
	prev := d.user
	d.user = policy.UserState(c, idle, game)
	if d.user != prev {
		logf("user: %s -> %s %s", prev, d.user, d.gameWhy)
		d.broadcast()
	}
}

// gpuHeld: the human's current state reserves the whole GPU (pause_gpu).
func (d *Daemon) gpuHeld() bool { return d.cfg.ReserveFor(d.user).PauseGPU }

// enforceCPU keeps jobs on the highest-numbered CPUs, leaving the reserve idle for the human
// (PerfIso: a resized idle-core buffer protects latency where priority and rate caps don't).
func (d *Daemon) enforceCPU() {
	n := max(1, d.snap.CPUs)
	keep := int(math.Round(d.res.CPUs))
	allow := max(1, n-keep)
	cpus := make([]int, 0, allow)
	for i := n - allow; i < n; i++ {
		cpus = append(cpus, i)
	}
	key := fmt.Sprint(allow)
	for _, j := range d.running() {
		for p, created := range j.pids {
			if j.applied[p] == key || !sysinfo.Same(p, created) {
				continue
			}
			if err := sysinfo.SetCPUs(p, cpus); err == nil {
				j.applied[p] = key
			}
		}
		if j.container != "" && j.applied[-1] != key {
			j.applied[-1] = key
			go docker("update", "--cpuset-cpus", fmt.Sprintf("%d-%d", n-allow, n-1), j.container)
		}
	}
}

// ---- preemption ladder ---------------------------------------------------------------------------

func dur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// leaseUsed is how long a job has run, not counting time the guard held it paused.
func (j *job) leaseUsed(now time.Time) time.Duration {
	used := now.Sub(j.Started) - j.pausedTotal
	if !j.pausedAt.IsZero() {
		used -= now.Sub(j.pausedAt)
	}
	return used
}

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

	// The human's state holds the GPU (game by default): cooperative pause -> suspend (bounded)
	// -> checkpoint, for GPU jobs. Jobs marked evict=never stay suspended instead.
	hold := d.gpuHeld()
	for _, j := range d.running() {
		if !j.GPU || !j.killedAt.IsZero() {
			continue
		}
		switch {
		case hold && j.pausedAt.IsZero() && j.checkpoint.IsZero():
			j.pausedAt, j.reason = now, "the human needs the GPU ("+d.why()+")"
			touch(PauseFile(j.ID))
			logf("%s: pause requested: %s", j.Name, j.reason)
		case hold && !j.pausedAt.IsZero() && j.suspended.IsZero() && j.checkpoint.IsZero() && !exists(PausedFile(j.ID)) &&
			now.Sub(j.pausedAt) > dur(c.Ladder.PauseAckSeconds):
			j.suspended = now
			d.each(j, sysinfo.Suspend)
			d.signalContainer(j, "pause")
			logf("%s: no pause ack in %.0fs, suspended", j.Name, c.Ladder.PauseAckSeconds)
		case hold && !j.suspended.IsZero() && j.checkpoint.IsZero() && j.Evict != "never" &&
			now.Sub(j.suspended) > dur(c.Ladder.SuspendMaxSeconds):
			d.requestCheckpoint(j, now, "the human still needs the GPU after the suspend limit")
		case !hold && !j.pausedAt.IsZero():
			d.unpause(j, now)
			logf("%s: resumed", j.Name)
		}
	}

	// RAM and VRAM pressure: under the reserve for PressureSeconds -> checkpoint one victim.
	ramFloor := d.snap.RAMTotalGB * d.res.RAMPct / 100
	ramShort := d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < ramFloor
	free, vt := d.snap.MaxFreeVRAM()
	vramShort := vt > 0 && free < d.res.VRAMGB
	d.ramLow = since(d.ramLow, ramShort, now)
	d.vramLow = since(d.vramLow, vramShort, now)
	if !d.ramLow.IsZero() && now.Sub(d.ramLow) > dur(c.Ladder.PressureSeconds) {
		if v := d.victim(false, true, func(j *job) float64 { return j.ramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("RAM below the human's %.0f%% reserve", d.res.RAMPct))
		}
	}
	if !d.vramLow.IsZero() && now.Sub(d.vramLow) > dur(c.Ladder.PressureSeconds) {
		if v := d.victim(true, true, func(j *job) float64 { return j.vramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("VRAM below the human's %.1f GB reserve", d.res.VRAMGB))
		}
	}
	// Critical RAM: kill at once, even mid-grace (lmkd/earlyoom order: highest band, heaviest first).
	if d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < d.snap.RAMTotalGB*c.Ladder.KillBelowRAMPct/100 {
		if v := d.victim(false, false, func(j *job) float64 { return j.ramGB }); v != nil {
			v.reason = "RAM critically low"
			d.killJob(v, now)
		}
	}

	// A spot GPU job yields to the head of the in-budget GPU line only when that helps: the slots
	// are full, the spot job has had its minimum runtime, and its VRAM is what the head lacks.
	if !hold {
		ram, vram := d.headroom(now)
		var head *policy.Ticket
		for _, t := range d.waiting() {
			if t.GPU && t.Band < policy.BandSpot {
				head = t
				break
			}
		}
		slotsFull := d.gpuBusySlots() >= c.GPU.MaxJobs
		if head != nil && head.NeedRAM <= ram {
			var spot *job
			for _, j := range d.running() {
				if j.GPU && j.Band == policy.BandSpot && j.checkpoint.IsZero() && j.Evict != "never" &&
					j.leaseUsed(now).Minutes() >= c.Ladder.MinRuntimeMinutes && (spot == nil || j.vramGB > spot.vramGB) {
					spot = j
				}
			}
			fits := head.NeedVRAM <= vram
			if spot != nil && (slotsFull || !fits) && head.NeedVRAM <= vram+spot.vramGB+0.5 {
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
		d.each(j, sysinfo.Resume)
		d.signalContainer(j, "unpause")
	}
	if !j.pausedAt.IsZero() {
		j.pausedTotal += now.Sub(j.pausedAt)
	}
	j.pausedAt, j.suspended, j.reason = time.Time{}, time.Time{}, ""
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

// victim picks the job to evict: highest band first, never-evict jobs last, heaviest first. With
// oneAtATime, nothing is picked while another eviction is still in its grace period.
func (d *Daemon) victim(gpuOnly, oneAtATime bool, weight func(*job) float64) *job {
	var best *job
	for _, j := range d.running() {
		if !j.killedAt.IsZero() {
			continue
		}
		if !j.checkpoint.IsZero() {
			if oneAtATime {
				return nil
			}
		}
		if gpuOnly && !j.GPU {
			continue
		}
		if best == nil || better(j, best, weight) {
			best = j
		}
	}
	return best
}

func better(a, b *job, w func(*job) float64) bool {
	if (a.Evict == "never") != (b.Evict == "never") {
		return b.Evict == "never"
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
	if !j.suspended.IsZero() {
		d.each(j, sysinfo.Resume)
		d.signalContainer(j, "unpause")
		j.suspended = time.Time{}
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
	if j.checkpoint.IsZero() {
		j.checkpoint = now
	}
	logf("%s: killed (%s)", j.Name, j.reason)
	if j.container != "" {
		go docker("kill", j.container)
	}
	if !j.suspended.IsZero() {
		d.each(j, sysinfo.Resume) // a stopped process can't die cleanly on some systems
	}
	d.each(j, sysinfo.Kill)
}

// each applies f to every member still the same process it was when it joined the job.
func (d *Daemon) each(j *job, f func(int) error) {
	for p, created := range j.pids {
		if sysinfo.Same(p, created) {
			_ = f(p)
		}
	}
}

func (d *Daemon) signalContainer(j *job, verb string) {
	if j.container != "" {
		go docker(verb, j.container)
	}
}

// reap closes tickets whose client is gone. A running job whose client died (killed agent,
// closed terminal) is stopped too, so it can't hold the GPU with nobody waiting for it.
func (d *Daemon) reap(now time.Time) {
	for id, j := range d.jobs {
		if j.State == "done" {
			continue
		}
		clientGone := j.ClientPID > 1 && !sysinfo.Alive(j.ClientPID)
		jobGone := j.PID <= 1 || !sysinfo.Same(j.PID, j.rootCreated)
		switch {
		case j.State == "granted" && clientGone:
			d.finish(id, -1, "lost", "")
		case j.State == "waiting" && clientGone:
			d.finish(id, -1, "cancelled", "")
		case j.State == "running" && j.adopted && jobGone:
			d.finish(id, -1, "ended", "")
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

// gpuBusySlots counts GPU slots taken: firefighter GPU jobs (running or granted) plus heavy GPU
// compute outside firefighter.
func (d *Daemon) gpuBusySlots() int {
	n := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") {
			n++
		}
	}
	for _, u := range d.foreign {
		if u >= d.cfg.Reserve.ForeignGPUPct {
			n++
		}
	}
	return n
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

// headroom is what jobs may still take: available minus the reserve minus what recently started
// jobs haven't grown into yet. VRAM is per card: the freest GPU.
func (d *Daemon) headroom(now time.Time) (ram, vram float64) {
	ram = d.snap.RAMAvailGB - d.snap.RAMTotalGB*d.res.RAMPct/100
	free, _ := d.snap.MaxFreeVRAM()
	vram = free - d.res.VRAMGB
	for _, j := range d.jobs {
		if j.State == "granted" || (j.State == "running" && now.Sub(j.Started) < 90*time.Second) {
			ram -= math.Max(0, j.NeedRAM-j.ramGB)
			if j.GPU {
				vram -= math.Max(0, j.NeedVRAM-j.vramGB)
			}
		}
	}
	return
}

func (d *Daemon) admit(now time.Time) {
	c := d.cfg
	ram, vram := d.headroom(now)
	_, vt := d.snap.MaxFreeVRAM()
	gpuRunning := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") {
			gpuRunning++
		}
	}
	gpuRunning = d.gpuBusySlots()
	gpuBlocked := d.gpuHeld()
	// Pack a second GPU job only while the smoothed (not momentary) utilization leaves room.
	if gpuRunning > 0 && math.Max(d.utilEWMA, d.snap.GPUUtil()*0.5) >= c.GPU.PackBelowUtil {
		gpuBlocked = true
	}
	changed := false
	headWaitsOnVRAM, cpuHeadBlocked := false, false
	for _, t := range d.waiting() {
		j := d.jobs[t.ID]
		if t.GPU {
			if gpuBlocked || gpuRunning >= c.GPU.MaxJobs {
				continue
			}
			if vt > 0 && t.NeedVRAM > vram {
				headWaitsOnVRAM = true // the head waits for VRAM; only short jobs may backfill
				continue
			}
			// EASY-style backfill: behind a head that waits on VRAM, only a short job that fits
			// now may start, so the head is delayed by at most ~10 minutes.
			if headWaitsOnVRAM && t.PredMinutes > 10 {
				continue
			}
		} else if cpuHeadBlocked && t.PredMinutes > 10 {
			continue // an overdue big CPU job is waiting for RAM: don't let long jobs eat it first
		}
		if t.NeedRAM > ram {
			if t.GPU {
				gpuBlocked = true
			} else if now.Sub(t.Created).Minutes() >= c.Order.MaxWaitMinutes {
				cpuHeadBlocked = true
			}
			continue
		}
		ram -= t.NeedRAM
		if t.GPU {
			vram -= t.NeedVRAM
			gpuRunning++
			j.backfilled = headWaitsOnVRAM
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

// ---- band 4: idle harvester ------------------------------------------------------------------------

func (d *Daemon) harvester(now time.Time) {
	h := d.cfg.Harvester
	if h.Start == "" {
		return
	}
	busy := d.gpuHeld()
	for _, j := range d.jobs {
		busy = busy || (j.GPU && j.State != "done")
	}
	if busy {
		d.harvest.quietFrom = time.Time{}
		if d.harvest.on {
			d.harvest.on = false
			logf("harvester: stop (GPU wanted)")
			go shell(h.Stop)
		}
		return
	}
	if d.harvest.quietFrom.IsZero() {
		d.harvest.quietFrom = now
	}
	if !d.harvest.on && now.Sub(d.harvest.quietFrom).Seconds() >= h.IdleSeconds {
		d.harvest.on = true
		logf("harvester: start (GPU idle %.0fs)", h.IdleSeconds)
		go shell(h.Start)
	}
}

func shell(cmd string) {
	if cmd == "" {
		return
	}
	argv := []string{"sh", "-c", cmd}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", cmd}
	}
	c := exec.Command(argv[0], argv[1:]...)
	sysinfo.PrepareChild(c, argv)
	if err := c.Start(); err != nil {
		logf("shell %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

func docker(args ...string) {
	c := exec.Command("docker", args...)
	sysinfo.PrepareChild(c, nil)
	if out, err := c.CombinedOutput(); err != nil {
		logf("docker %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// ---- helpers ------------------------------------------------------------------------------------

func (d *Daemon) prune(now time.Time) {
	for id, r := range d.st.Receipts {
		if now.Sub(r.Ended) > 48*time.Hour {
			delete(d.st.Receipts, id)
		}
	}
	for id, j := range d.jobs {
		if j.State == "done" && now.Sub(j.receipt.Ended) > 10*time.Minute {
			delete(d.jobs, id)
		}
	}
	var keep []*Plan
	for _, p := range d.st.Plans {
		if now.Sub(p.Added) < 24*time.Hour {
			keep = append(keep, p)
		}
	}
	d.st.Plans = keep
	if ents, err := os.ReadDir(filepath.Join(config.Home(), "logs")); err == nil {
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 7*24*time.Hour {
				_ = os.Remove(filepath.Join(config.Home(), "logs", e.Name()))
			}
		}
	}
}

func touch(p string) { _ = os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)), 0o644) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

var logMu sync.Mutex

func logf(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	path := filepath.Join(config.Home(), "daemon.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 { // keep one 10 MB generation
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
