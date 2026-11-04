// Package daemon is the guard: one per user per machine. It samples the machine twice a second,
// sizes the human's reserve, admits waiting jobs in policy order, charges budgets, and walks the
// preemption ladder when the human needs resources back.
package daemon

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
	rootCreated  int64
	clientAt     int64          // the client's start time (with ClientPID: one process, for good)
	human        bool           // the human created it: only the human (or --force) changes it
	pids         map[int]int64  // member pid -> start time (pid + start time = one process, for good)
	applied      map[int]string // pid -> cpu set applied
	cpuPrev      float64
	cpuNow       float64 // CPUs in use
	ramGB        float64
	vramGB       float64
	gpuUtil      float64
	peakVRAM     float64
	peakRAM      float64
	charged      float64
	overrun      bool
	granted      time.Time
	pausedAt     time.Time // cooperative pause flag set
	pausedTotal  time.Duration
	suspended    time.Time
	checkpoint   time.Time // checkpoint-and-exit requested
	termAt       time.Time
	killedAt     time.Time
	reason       string
	container    string
	receipt      *Receipt
	extended     float64
	price        float64   // locked at grant: bid price x congestion x surcharge
	adopted      bool      // auto-adopted: heavy work an agent started outside firefighter
	backfilled   bool      // started ahead of a GPU head waiting on VRAM: can't extend its lease
	device       int       // GPU index the job was placed on (-1: any/unknown)
	engine       string    // container engine of j.container: docker | podman
	ownerAgent   int       // pid of the agent session that submitted it (0: a human)
	ownerCreated int64     // its start time (a reused pid isn't the same session)
	ackBusy      time.Time // acked the pause but still computing on the GPU since then
}

func newJob(t *policy.Ticket) *job {
	j := &job{Ticket: t, pids: map[int]int64{}, applied: map[int]string{}, device: -1}
	if t.PID > 1 {
		j.rootCreated = sysinfo.Created(t.PID)
		if j.rootCreated != 0 {
			j.pids[t.PID] = j.rootCreated
		}
	}
	return j
}

type Daemon struct {
	mu            sync.Mutex
	cfg           config.Config
	cfgShared     atomic.Pointer[config.Config] // the same settings, for lookups done outside the lock
	cfgMod        time.Time
	cfgErr        string
	st            saved
	jobs          map[string]*job
	reader        *sysinfo.Reader
	snap          sysinfo.Snapshot
	res           policy.ReserveCtl
	user          string
	gameWhy       string
	gameTill      time.Time
	busyUser      float64
	utilEWMA      float64   // GPU utilization smoothed over ~30 s: packing decisions ignore momentary dips
	cardUtil      []float64 // the same, per card: each card packs (and takes turns) on its own
	ramLow        time.Time
	vramLow       time.Time
	names         map[string]bool
	namesAt       time.Time
	foreign       map[string]float64   // non-job GPU users -> %
	outSince      map[string]time.Time // when each busy non-job GPU user was first seen busy
	lastKill      time.Time            // critical-RAM kills wait for the last one to free its memory
	stopping      atomic.Bool          // shutdown requested (once)
	unknownAt     map[bool]time.Time   // last grant of a never-seen command, per kind (GPU/CPU)
	cardFloor     []float64            // the least VRAM each card has had in use lately: the emptiest it gets
	gpuSeen       bool                 // a GPU has reported since start (a missed reading isn't "no GPU")
	netCap        float64              // the most network throughput seen (Mbps), slowly forgotten: the link's size
	netHuman      float64              // network use while no network job runs (Mbps, smoothed): the human's
	netQuietFrom  time.Time            // when the last network job ended, or the daemon started (the human's traffic is measured after)
	netWasBusy    bool                 // a network job ran last tick
	prevOpen      map[string]OpenJob   // running jobs the previous daemon granted, until re-registered
	backoff       map[string]backoff   // job id -> evictions in a row: a requeued job waits longer each time
	agentOf       map[int]agentSeen    // heavy processes an agent started, remembered past their parents' exit
	gpuMax        int                  // most GPUs seen in one reading
	gpuShortSince time.Time            // since when readings show fewer
	gpuDegraded   bool                 // this reading is partial: no GPU grants
	dockerQ       chan []string        // docker CLI calls, run in order (a late pause must not follow its unpause)
	graphics      map[string]float64   // non-job 3D-engine users -> % (games, video)
	others        []sysinfo.Container
	notify        chan struct{}
	rng           *rand.Rand
	harvest       struct {
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
		jobs:         map[string]*job{},
		reader:       sysinfo.NewReader(),
		st:           load(),
		notify:       make(chan struct{}),
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
		started:      time.Now(),
		netQuietFrom: time.Now(), // measure the human's traffic before the first download
		user:         "present",
		Version:      version,
		stop:         make(chan struct{}),
	}
	d.reloadConfig()
	if os.Getenv("FIREFIGHTER_FAKE_FILE") != "" && config.IsDefaultHome() {
		// Scripted readings are for tests in their own home: the real guard (the one every client
		// uses) always reads the real machine, so no one can blind it to the human.
		_ = os.Unsetenv("FIREFIGHTER_FAKE_FILE")
		logf("FIREFIGHTER_FAKE_FILE ignored: fake readings only work with a separate FIREFIGHTER_HOME")
	}
	if runtime.GOOS != "windows" {
		// Tools the daemon runs to tell who's calling (lsof on macOS) come from the system, never
		// from a folder an agent could put first on the PATH it was started with.
		_ = os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	}
	// Jobs the previous daemon granted: their clients may re-register them as running.
	d.prevOpen, d.st.Open = d.st.Open, nil
	_ = os.MkdirAll(signalDir(), 0o700)
	// Pause files left by a daemon that died mid-game would freeze cooperative jobs: clear them.
	if ents, err := os.ReadDir(signalDir()); err == nil {
		for _, e := range ents {
			_ = os.Remove(filepath.Join(signalDir(), e.Name()))
		}
	}
	// Processes a previous daemon suspended and never resumed (it crashed or was killed mid-game):
	// thaw them, whoever started them.
	for p, created := range d.st.Frozen {
		if sysinfo.Same(p, created) {
			_ = sysinfo.Resume(p)
		}
	}
	d.st.Frozen = map[int]int64{}
	d.dockerQ = make(chan []string, 64)
	go func() {
		for args := range d.dockerQ {
			docker(args[0], args[1:]...)
		}
	}()
	// The harvester's state isn't known after a restart: stop it; it restarts after the idle wait.
	go shell(d.cfg.Harvester.Stop)
	return d
}

// openJobs lists the running jobs this guard granted (not adopted ones), plus those the previous
// daemon granted whose clients haven't re-registered yet (kept 10 minutes, across quick restarts).
func (d *Daemon) openJobs(now time.Time) map[string]OpenJob {
	open := map[string]OpenJob{}
	for id, o := range d.prevOpen {
		if now.Sub(o.Seen) < 10*time.Minute {
			open[id] = o
		}
	}
	for id, j := range d.jobs {
		if j.State == "running" && !j.adopted && j.ClientPID > 1 {
			open[id] = OpenJob{Client: j.ClientPID, ClientAt: j.clientAt, Seen: now}
		}
	}
	return open
}

// waitsForAway: a GPU job that needs more than its card frees beside the human's reserve would
// only be stopped again; it gets the card at its emptiest while the human is away.
func (d *Daemon) waitsForAway(t *policy.Ticket) bool {
	if d.nobodyAtKeyboard() || time.Since(t.Created).Minutes() >= d.cfg.Order.MaxWaitMinutes {
		return false // no one to wait for, or it waited its 45 minutes
	}
	card, _ := d.cardFor(t)
	return card >= 0 && d.needOn(t, card) < t.NeedVRAM-0.05
}

// nobodyAtKeyboard: the human is away, or input can't be read at all (a headless box, a desktop
// without an idle source) and no game or fullscreen app is up: there's no one to make room for.
func (d *Daemon) nobodyAtKeyboard() bool {
	return d.user == "away" || (d.snap.Idle < 0 && d.user != "game")
}

// backoff: how often in a row a job was stopped for the human, and when it last was.
type backoff struct {
	n  int
	at time.Time
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
	_ = d.st.write() // now, not at the next periodic save: a crash in between would strand them
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
			d.cfgShared.Store(&d.cfg)
		}
		return
	}
	d.cfgErr = ""
	d.cfg = c
	d.cfgShared.Store(&c)
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
	if dt > 30*time.Second {
		// A tick 30 s late: the machine slept (or the daemon was frozen). Nothing ran meanwhile, so
		// it isn't lease time.
		for _, j := range d.running() {
			if j.pausedAt.IsZero() {
				j.pausedTotal += dt // a paused job's pause already covers it
			}
			if !j.checkpoint.IsZero() {
				j.checkpoint = j.checkpoint.Add(dt) // its grace to save and exit starts over from now
			}
			if !j.termAt.IsZero() {
				j.termAt = j.termAt.Add(dt)
			}
		}
		dt = 0
	}
	if dt > 0 {
		d.utilEWMA += (1 - math.Exp(-dt.Seconds()/30)) * (snap.GPUUtil() - d.utilEWMA)
	} else {
		d.utilEWMA = snap.GPUUtil()
	}
	if len(d.cardUtil) != len(snap.GPUs) {
		d.cardUtil = make([]float64, len(snap.GPUs))
		for i, g := range snap.GPUs {
			d.cardUtil[i] = g.Util
		}
	} else {
		for i, g := range snap.GPUs {
			d.cardUtil[i] += (1 - math.Exp(-dt.Seconds()/30)) * (g.Util - d.cardUtil[i])
		}
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
		d.st.Open = d.openJobs(now)
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
				if o.ID != "" && strings.HasPrefix(j.container, o.ID) {
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
		if jobPIDs[pid] || (d.snap.ProcUtilKnown && g.Util < 1) || (!d.snap.ProcUtilKnown && g.VRAMGB < 1) {
			continue
		}
		n := sysinfo.Name(pid)
		if n == "" || contains(c.Reserve.IgnoreGPUApps, n) {
			continue
		}
		if d.containerGPUJob() && (strings.HasPrefix(strings.ToLower(n), "vmmem") || underContainerRuntime(pid)) {
			// A container job's GPU work, seen through the WSL VM (Windows) or under the runtime's
			// shim (Linux): firefighter's own, not an outsider's.
			continue
		}
		u := g.Util
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

// underContainerRuntime says whether a process runs inside a container (a container runtime's
// shim is among its ancestors).
func underContainerRuntime(pid int) bool {
	for _, a := range sysinfo.Ancestors(pid) {
		n := strings.ToLower(sysinfo.Name(a))
		if strings.HasPrefix(n, "containerd-shim") || n == "conmon" || n == "crun" || n == "runc" {
			return true
		}
	}
	return false
}

// containerGPUJob says whether a GPU job is running in a container.
func (d *Daemon) containerGPUJob() bool {
	for _, j := range d.running() {
		if j.GPU && j.container != "" {
			return true
		}
	}
	return false
}

func anyGPUJobRunning(d *Daemon) bool {
	for _, j := range d.running() {
		if j.GPU {
			return true
		}
	}
	return false
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
		if gain := a.Bucket - before; gain > 0 {
			got := math.Min(gain, d.newcomers(now))
			a.Bucket = before + got
			d.st.Newcomers -= got
		}
	}
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
			if runtime.GOOS == "linux" {
				d.docker(j.engine, "update", "--cpuset-cpus", fmt.Sprintf("%d-%d", n-allow, n-1), j.container)
			} else { // Docker Desktop runs containers in a VM whose CPU numbers aren't the host's: cap the count
				d.docker(j.engine, "update", "--cpus", strconv.Itoa(allow), j.container)
			}
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

	// Memory pressure, measured first: the game ladder below only evicts suspended jobs when the
	// memory they hold is what the human is short of.
	ramFloor := d.snap.RAMTotalGB * d.res.RAMPct / 100
	ramShort := d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < ramFloor
	vramCard := d.vramShortCard()
	d.ramLow = since(d.ramLow, ramShort, now)
	d.vramLow = since(d.vramLow, vramCard >= 0, now)

	// The human's state holds the GPU (game by default): cooperative pause, then suspend. A job
	// stays suspended while the human needs the GPU; it is only evicted if the memory it holds is
	// what's short (and --evict never jobs never are, short of critical RAM).
	gpuHold, netHold := d.gpuHeld(), d.user == "game" && c.Network.PauseInGame
	for _, j := range d.running() {
		if (!j.GPU && !j.Net) || !j.killedAt.IsZero() {
			continue
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
		case hold && !j.suspended.IsZero() && j.checkpoint.IsZero() && j.Evict != "never" && j.GPU &&
			now.Sub(j.suspended) > dur(c.Ladder.SuspendMaxSeconds) &&
			((ramShort && j.ramGB > 0.5) || (vramCard >= 0 && d.onCard(j, vramCard))):
			// A GPU job suspended past the limit and holding memory the human is short of. It ignored
			// the pause file, so it can't checkpoint: resuming it would only let it compute through
			// the game. Stop it (requeued). A download frozen for the game is left to the RAM
			// pressure rule, which picks the job whose RAM covers the shortfall and thaws it to save.
			j.reason = "suspended past the limit, holding memory the human needs"
			d.killJob(j, now)
		case !hold && !j.pausedAt.IsZero():
			d.unpause(j, now)
			logf("%s: resumed", j.Name)
		}
	}

	// RAM and VRAM pressure: under the reserve for PressureSeconds -> checkpoint one victim, the one
	// whose leaving actually covers the shortfall.
	if !d.ramLow.IsZero() && now.Sub(d.ramLow) > dur(c.Ladder.PressureSeconds) {
		short := ramFloor - d.snap.RAMAvailGB
		if v := d.victim(nil, true, false, short, func(j *job) float64 { return j.ramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("RAM below the human's %.0f%% reserve", d.res.RAMPct))
		}
	}
	if !d.vramLow.IsZero() && now.Sub(d.vramLow) > dur(c.Ladder.PressureSeconds) {
		card := vramCard
		short := d.res.VRAMGB
		if card >= 0 && card < len(d.snap.GPUs) {
			g := d.snap.GPUs[card]
			short = math.Max(0.1, d.res.VRAMGB-(g.VRAMGB-g.UsedGB)) // what the human is missing, not the whole reserve
		}
		if v := d.victim(func(j *job) bool { return j.GPU && d.onCard(j, card) }, true, false, short, func(j *job) float64 { return j.vramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("VRAM below the human's %.1f GB reserve", d.res.VRAMGB))
		}
	}
	// Critical RAM: kill at once, even mid-grace: the job whose RAM covers the shortfall (or the
	// heaviest), one at a time, giving each kill a few seconds to actually free its memory.
	if d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < d.snap.RAMTotalGB*c.Ladder.KillBelowRAMPct/100 && now.Sub(d.lastKill) > 5*time.Second {
		short := d.snap.RAMTotalGB*c.Ladder.KillBelowRAMPct/100 - d.snap.RAMAvailGB
		if v := d.victim(nil, false, true, short, func(j *job) float64 { return j.ramGB }); v != nil {
			v.reason = "RAM critically low"
			d.killJob(v, now)
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
	j.pausedAt, j.suspended, j.ackBusy, j.reason = time.Time{}, time.Time{}, time.Time{}, ""
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

// vramShortCard returns the GPU whose free VRAM is under the human's reserve while firefighter
// jobs run on it, or -1.
func (d *Daemon) vramShortCard() int {
	waitingGPU := false
	for _, t := range d.waiting() {
		if t.GPU {
			waitingGPU = true
		}
	}
	// Away with nobody waiting for VRAM: a job that fills the card by design (TensorFlow's default)
	// hurts no one, so don't evict it in a loop.
	if d.nobodyAtKeyboard() && !waitingGPU {
		return -1
	}
	for i, g := range d.snap.GPUs {
		if g.VRAMGB-g.UsedGB >= d.res.VRAMGB {
			continue
		}
		for _, j := range d.running() {
			if j.GPU && d.onCard(j, i) {
				return i
			}
		}
	}
	return -1
}

// onCard says whether a job runs (or may run) on GPU i. Jobs placed nowhere in particular use the
// default device, 0.
func (d *Daemon) onCard(j *job, i int) bool {
	if j.device >= 0 {
		return j.device == i
	}
	return i == 0 || len(d.snap.GPUs) <= 1
}

// requestCheckpoint asks a job to save and exit; a suspended job is resumed first so it can.
func (d *Daemon) requestCheckpoint(j *job, now time.Time, why string) {
	if !j.checkpoint.IsZero() || !j.killedAt.IsZero() {
		return
	}
	if !j.suspended.IsZero() && j.Net && !j.GPU {
		// A download frozen for a game (never asked to pause): thaw it so it can save and exit in
		// its grace period like any job; a few seconds of traffic beat losing the download.
		d.resume(j)
		j.suspended = time.Time{}
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

func (d *Daemon) signalContainer(j *job, verb string) {
	if j.container != "" {
		d.docker(j.engine, verb, j.container)
	}
}

// docker queues a container CLI call (engine first: docker or podman); calls run one at a time,
// in order.
func (d *Daemon) docker(engine string, args ...string) {
	if engine != "podman" {
		engine = "docker"
	}
	select {
	case d.dockerQ <- append([]string{engine}, args...):
	default:
		logf("container queue full; dropped: %s %s", engine, strings.Join(args, " "))
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

// headroom is what jobs may still take: available minus the reserve minus what granted and running
// jobs haven't grown into yet (a job loading data for minutes hasn't allocated its model; a job
// that freed its cache while paused will take it back). VRAM is per card: the freest GPU, less the
// unclaimed needs of jobs placed on it.
func (d *Daemon) headroom(now time.Time) (ram, vram float64) {
	ram = d.snap.RAMAvailGB - d.snap.RAMTotalGB*d.res.RAMPct/100
	card, free := d.freestCard()
	vram = free - d.res.VRAMGB
	for _, j := range d.jobs {
		if j.State == "granted" || j.State == "running" {
			ram -= math.Max(0, j.NeedRAM-j.ramGB)
			if j.GPU && d.onCard(j, card) {
				vram -= math.Max(0, j.NeedVRAM-j.vramGB)
			}
		}
	}
	return
}

// freestCard is the GPU with the most free VRAM after its jobs' unclaimed needs, and its raw free
// VRAM (-1, 0 with no GPU).
func (d *Daemon) freestCard() (int, float64) {
	best, bestLeft, bestFree := -1, 0.0, 0.0
	for i, g := range d.snap.GPUs {
		free := g.VRAMGB - g.UsedGB
		left := free
		for _, j := range d.jobs {
			if j.GPU && (j.State == "granted" || j.State == "running") && d.onCard(j, i) {
				left -= math.Max(0, j.NeedVRAM-j.vramGB)
			}
		}
		if best < 0 || left > bestLeft {
			best, bestLeft, bestFree = i, left, free
		}
	}
	return best, bestFree
}

// cardFor picks the GPU a job should use: among the vendor it wants (see policy.Ticket.WantVendor),
// the one with the most VRAM left after jobs' unclaimed needs. Returns -1 with no GPU.
// vendorFor resolves which GPUs a job can use: its --gpu-vendor, else NVIDIA when there is one
// (most GPU code is CUDA), else any ("").
func (d *Daemon) vendorFor(want string) string {
	want = strings.ToLower(want)
	if want == "any" {
		return ""
	}
	if want == "" {
		for _, g := range d.snap.GPUs {
			if g.Vendor == "nvidia" {
				return "nvidia"
			}
		}
	}
	return want
}

// capacityOf is the most VRAM card i ever offers: its size, less the human's reserve, less the
// least others have held lately.
func (d *Daemon) capacityOf(i int) float64 {
	g := d.snap.GPUs[i]
	floor := 0.0
	if i < len(d.cardFloor) {
		floor = d.cardFloor[i]
	}
	return g.VRAMGB - d.res.VRAMGB - floor
}

// capFor is how much of card i a ticket can count on: for a need its own --vram covers, the card's
// size less the human's reserve (it waits for others to leave); for a learned need, what the card
// has actually freed lately (capacityOf).
func (d *Daemon) capFor(i int, t *policy.Ticket) float64 {
	if t.VRAMGB > 0 && t.NeedVRAM <= t.VRAMGB {
		return d.snap.GPUs[i].VRAMGB - d.res.VRAMGB
	}
	return d.capacityOf(i)
}

// cardFor picks the GPU for a ticket: among the cards of the vendor it can use, the freest one that
// can hold it (capFor); if none can, the one that offers the most (needOn then clamps the need to
// it). Returns the card and its VRAM left after jobs' unclaimed needs; -1 with no card.
func (d *Daemon) cardFor(t *policy.Ticket) (int, float64) {
	want, need := t.WantVendor, t.NeedVRAM
	v := d.vendorFor(want)
	best, bestLeft, bestFits, bestOpen := -1, 0.0, false, false
	for i, g := range d.snap.GPUs {
		if v != "" && g.Vendor != v {
			continue
		}
		left := g.VRAMGB - g.UsedGB
		for _, j := range d.jobs {
			if j.GPU && (j.State == "granted" || j.State == "running") && d.onCard(j, i) {
				left -= math.Max(0, j.NeedVRAM-j.vramGB)
			}
		}
		fits, open := d.capFor(i, t) >= need, d.cardOpen(i)
		switch {
		case best < 0, fits && !bestFits: // a card that can hold it first: it waits for that one
		case fits != bestFits:
			continue
		case open && !bestOpen: // then one that takes a job now
		case open != bestOpen:
			continue
		case fits && d.lighter(i, best, left, bestLeft): // among cards that fit: the least loaded
		case !fits && d.capFor(i, t) > d.capFor(best, t): // none fits: the biggest
		default:
			continue
		}
		best, bestLeft, bestFits, bestOpen = i, left, fits, open
	}
	return best, bestLeft
}

// lighter says whether card a is a better place for a new job than card b: fewer firefighter jobs,
// then less smoothed load, then more VRAM left.
func (d *Daemon) lighter(a, b int, leftA, leftB float64) bool {
	if ja, jb := d.cardJobs(a), d.cardJobs(b); ja != jb {
		return ja < jb
	}
	if ua, ub := d.utilOf(a), d.utilOf(b); math.Abs(ua-ub) >= 10 {
		return ua < ub
	}
	return leftA > leftB
}

// cardJobs counts the GPU jobs running or granted on card i.
func (d *Daemon) cardJobs(i int) int {
	n := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") && d.onCard(j, i) {
			n++
		}
	}
	return n
}

// utilOf is card i's smoothed utilization (its momentary reading counts at half: a dip isn't room).
func (d *Daemon) utilOf(i int) float64 {
	u := 0.0
	if i < len(d.cardUtil) {
		u = d.cardUtil[i]
	}
	if i < len(d.snap.GPUs) {
		u = math.Max(u, d.snap.GPUs[i].Util*0.5)
	}
	return u
}

// cardOpen says whether card i takes another job now: fewer than gpu.max_jobs on it (heavy outside
// load on an otherwise empty card counts as one), and a busy card packs a second job only while
// its smoothed utilization leaves room.
func (d *Daemon) cardOpen(i int) bool {
	c := d.cfg
	n := d.cardJobs(i)
	if n == 0 && d.utilOf(i) >= c.Reserve.ForeignGPUPct {
		n = 1
	}
	return n < c.GPU.MaxJobs && !(n > 0 && d.utilOf(i) >= c.GPU.PackBelowUtil)
}

// cardSize is the VRAM of the card a job runs on (fallback: the machine's), for billing.
func (d *Daemon) cardSize(j *job, fallback float64) float64 {
	for i, g := range d.snap.GPUs {
		if d.onCard(j, i) {
			return g.VRAMGB
		}
	}
	return fallback
}

// needOn is a job's VRAM need on a card. A learned need (peak x margin) can't exceed what the card
// ever offers: its size, less the human's reserve, less the least others have held lately. Above
// that it would wait forever, so it's clamped and starts when the card is at its emptiest (if a
// program holds VRAM for good, the job runs with what's there and its log says so). A declared
// --vram is never shrunk.
func (d *Daemon) needOn(t *policy.Ticket, card int) float64 {
	need := t.NeedVRAM
	if card >= 0 && card < len(d.snap.GPUs) && need > d.capFor(card, t) {
		// More than this card can ever give it (no card can: cardFor prefers one that can). It
		// starts when the card is at its emptiest instead of waiting forever; queue time already
		// refused a --vram larger than every card.
		need = math.Max(0.5, d.capacityOf(card))
	}
	return need
}

// netMeasure is how long the human's own traffic is measured, with no network job running, before
// the next download may start (after the last one ends, and after the daemon starts).
const netMeasure = 30 * time.Second

// netHumanOnly: the only thing holding network jobs back is the human's measured share of the link
// (its size is learned from peaks, so it may be too small until a download has run once).
func (d *Daemon) netHumanOnly() bool {
	return strings.HasPrefix(d.netWhy(), "network: the human is using")
}

// netFree says whether a network job may start now: a network turn is free, no game holds the
// machine, and the human isn't already using much of the link.
func (d *Daemon) netFree() bool { return d.netWhy() == "" }

// netWhy says what holds a network job back ("" when nothing does). Between two downloads it waits
// netMeasure (30 s), so the human's own traffic is measured again (it can't be told apart while one runs).
func (d *Daemon) netWhy() string {
	c := d.cfg.Network
	n := 0
	for _, j := range d.jobs {
		if j.Net && (j.State == "running" || j.State == "granted") {
			n++
		}
	}
	switch {
	case c.MaxJobs > 0 && n >= c.MaxJobs:
		return "network turn: another download has it"
	case d.user == "game" && c.PauseInGame:
		return "network: the human is gaming (latency)"
	case n == 0 && time.Since(d.netQuietFrom) < netMeasure:
		return "network: measuring the human's own traffic first (30 s)"
	case d.netCap > 1 && d.netHuman > d.netCap*c.HumanBusyPct/100:
		return fmt.Sprintf("network: the human is using %.0f of ~%.0f Mbps", d.netHuman, d.netCap)
	}
	return ""
}

// cardLeft is the VRAM a job of this vendor would find on its card, after the human's reserve.
func (d *Daemon) cardLeft(t *policy.Ticket) float64 {
	_, left := d.cardFor(t)
	return left - d.res.VRAMGB
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
			card, left := d.cardFor(t) // already less the jobs granted this tick
			if card < 0 && len(d.snap.GPUs) > 0 {
				continue // no card of the vendor it needs right now
			}
			if card >= 0 && !d.cardOpen(card) {
				continue // every card it can use has its turns taken (gpu.max_jobs each) or is busy
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
		if t.NeedRAM > ram && !(overdue && jobsOn(false) == 0) {
			// Short of RAM. An overdue job with no other job running gets what there is (the ladder
			// still protects the human); otherwise it holds the line only once it's overdue.
			if overdue {
				if t.GPU {
					gpuBlocked = true
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
			j.backfilled = headWaitsOnVRAM[card]
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
	if !d.harvest.on && len(d.outSince) > 0 {
		busy = true // heavy GPU work outside firefighter (a render, a local model) is the human's
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
	sysinfo.PrepareBackground(c)
	if err := c.Start(); err != nil {
		logf("shell %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

func docker(engine string, args ...string) {
	// A hung engine (Docker Desktop restarting) must not block the queue behind it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, engine, args...)
	sysinfo.PrepareBackground(c)
	if out, err := c.CombinedOutput(); err != nil {
		logf("%s %s: %v %s", engine, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// ---- helpers ------------------------------------------------------------------------------------

func (d *Daemon) prune(now time.Time) {
	// Projects idle for a month and commands not run in three months are forgotten, so state.json
	// can't grow forever. Projects with a configured budget are kept.
	for name, a := range d.st.Accounts {
		if _, set := d.cfg.Budget.Projects[name]; !set && !a.DecayAt.IsZero() && now.Sub(a.DecayAt) > 30*24*time.Hour {
			open := false
			for _, j := range d.jobs {
				open = open || (j.Project == name && j.State != "done")
			}
			if !open {
				delete(d.st.Accounts, name)
			}
		}
	}
	for k, h := range d.st.History {
		if !h.Last.IsZero() && now.Sub(h.Last) > 90*24*time.Hour {
			delete(d.st.History, k)
		}
	}
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

func touch(p string) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p) // a link planted in signals/ is replaced, never written through
	}
	_ = os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)), 0o600)
}

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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
