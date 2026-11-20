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
	"github.com/JoshWright22/firefighter/internal/harvest"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

const tick = 500 * time.Millisecond

type job struct {
	*policy.Ticket
	rootCreated  int64
	clientAt     int64          // the client's start time (with ClientPID: one process, for good)
	human        bool           // the human created it: only the human (or --force) changes it
	pids         map[int]int64  // member pid -> start time (pid + start time = one process, for good)
	applied      map[int]string // pid -> cpu set applied
	cpuNow       float64        // CPUs in use
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
	// held: the guard suspended it to free a resource the human is short of ("ram" | "disk"); it
	// resumes where it stopped once there is room again. Nothing it computed is lost.
	held       string
	heldAt     time.Time
	heldDrive  string          // disk: the drive that ran low
	heldRAM    float64         // RAM it held when suspended: what it takes back on resume
	heldTotal  time.Duration   // all the time it has spent held
	holds      int             // times held: each one waits longer before resuming
	okSince    time.Time       // since when there has been room to resume it
	freedAt    time.Time       // suspended for RAM then: what that freed is measured a moment later
	freedGB    float64         // RAM its suspension (or its kill) gave back
	trimFailed bool            // suspending it did not free its RAM (locked or shared memory)
	cpuByPID   map[int]float64 // CPU seconds per member at the last reading
	written    float64         // GB its members have written so far
	writeRate  float64         // MB/s it writes, smoothed over ~10 s
	// For learning (usage histograms, one sample per run): time it really worked, and over that
	// time its CPU-seconds, GPU-percent-seconds and (network jobs) megabit-seconds.
	workSec, cpuSec, gpuSec float64
	netSum, netSec          float64
	written0                float64 // GB already written when first measured
	measured                bool
	oom                     bool // stopped for lack of RAM: its need is above the peak it reached
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
	ramBase       float64              // the least RAM in use outside jobs lately: the machine never frees it
	busyHist      [3]float64           // the last readings of non-job CPU use: the reserve follows the least (a one-tick spike isn't demand)
	busyN         int
	ramCrit       time.Time  // since when RAM has been critically low
	ramLowLast    time.Time  // the last time RAM was under the reserve
	ramBlocked    bool       // a waiting job lacks only RAM (admission found it so)
	lastHold      time.Time  // jobs are suspended (and resumed) one at a time, a few seconds apart
	critSaid      time.Time  // last "nothing to stop would help" log line
	vm            sysinfo.VM // the local Linux VM's memory (WSL2 / Docker Desktop), as last read
	vmAt          time.Time  // when
	vmWanted      int        // read the VM's own numbers (it runs a command inside it): 0 no, 1 a job waits for RAM, 2 RAM is tight
	vmBusy        atomic.Bool
	reclaim       reclaimState
	disks         map[string]sysinfo.Disk // drives jobs and the system use, by drive
	driveOf       map[string]string       // folder -> its drive
	sysDrives     map[string]bool         // the drives temp files and firefighter's own state live on
	diskLow       map[string]time.Time    // since when each drive has been under the reserve
	diskBusy      atomic.Bool
	humanAt       time.Time    // last sample of the human's use (every 5 s)
	ramRise       usage.Growth // the human's RAM over the last 5 minutes: how far it rises
	vramRise      usage.Growth
	stopping      atomic.Bool        // shutdown requested (once)
	unknownAt     map[bool]time.Time // last grant of a never-seen command, per kind (GPU/CPU)
	cardFloor     []float64          // the least VRAM each card has had in use lately: the emptiest it gets
	gpuSeen       bool               // a GPU has reported since start (a missed reading isn't "no GPU")
	netCap        float64            // the most network throughput seen (Mbps), slowly forgotten: the link's size
	netHuman      float64            // network use while no network job runs (Mbps, smoothed): the human's
	netQuietFrom  time.Time          // when the last network job ended, or the daemon started (the human's traffic is measured after)
	netWasBusy    bool               // a network job ran last tick
	prevOpen      map[string]OpenJob // running jobs the previous daemon granted, until re-registered
	backoff       map[string]backoff // job id -> evictions in a row: a requeued job waits longer each time
	agentOf       map[int]agentSeen  // heavy processes an agent started, remembered past their parents' exit
	gpuMax        int                // most GPUs seen in one reading
	gpuShortSince time.Time          // since when readings show fewer
	gpuDegraded   bool               // this reading is partial: no GPU grants
	dockerQ       chan []string      // docker CLI calls, run in order (a late pause must not follow its unpause)
	graphics      map[string]float64 // non-job 3D-engine users -> % (games, video)
	others        []sysinfo.Container
	notify        chan struct{}
	rng           *rand.Rand
	harvest       struct {
		on        bool
		quietFrom time.Time
		book      *harvest.Book // the profit guard's ledger (nil until the guard is configured)
		check     harvest.Check // its last verdict
		checkAt   time.Time     // when it was made
		accAt     time.Time     // last ledger accrual
		cpuBase   float64       // machine CPU busy while waiting to start (the harvester's CPU is what's above it)
		runAt     time.Time     // last look for a harvester that started itself
		probing   atomic.Bool   // that look is running
	}
	// owned by the Run goroutine only
	scanner   *sysinfo.Scanner
	rogueAt   time.Time
	rogueSeen map[int]time.Time
	dockerAt  time.Time
	docking   atomic.Bool
	vmProbeAt time.Time
	vmGuestAt time.Time
	diskAt    time.Time

	saveAt     time.Time
	statusAt   time.Time
	started    time.Time
	Version    string
	Exe        string      // the program's own path, as started (an update replaces the file there)
	restart    atomic.Bool // stopping to come back as a newer version
	updateNote string      // a newer release is waiting, or the last update failed
	Token      string
	Port       int
	stop       chan struct{}
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
	members   map[int]int64
	cpuSec    map[int]float64 // per member
	ramGB     float64
	writtenGB float64
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
		vmWanted := d.vmWanted
		folders := []string{os.TempDir(), config.Home()}
		for _, j := range d.jobs {
			if j.State != "done" && j.Cwd != "" {
				folders = append(folders, j.Cwd)
			}
		}
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
			cpu, ram, written := sysinfo.Usage(pids)
			meas[r.id] = measured{m, cpu, ram, written}
		}
		rogues := d.findRogues(now, cfg, snap, governed)
		d.refreshDocker(now)
		d.refreshVM(now, vmWanted)
		d.refreshDisks(now, folders)

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
	d.observeHuman(now, dt)
	d.adoptRogues(rogues, now)
	d.res.Update(d.withLearnedReserve(c, now), d.user, d.busyUser, d.snap.CPUs, now)
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
		case hold && !j.suspended.IsZero() && j.checkpoint.IsZero() && j.Evict != "never" && j.GPU &&
			now.Sub(j.suspended) > dur(c.Ladder.SuspendMaxSeconds) &&
			((ramShort && j.ramGB > 0.5 && (!sysinfo.CanTrim || j.trimFailed)) || (vramCard >= 0 && d.onCard(j, vramCard))):
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
		if v := d.victim(func(j *job) bool { return j.GPU && d.onCard(j, card) }, true, false, short, func(j *job) float64 { return j.vramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("VRAM below the human's %.1f GB reserve", d.res.VRAMGB))
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
// --vram is measured against the card's size less the human's reserve; past that it's clamped the
// same way and waits for the human to be away.
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

// ---- band 4: idle harvester ------------------------------------------------------------------------

// harvesterProc says whether a GPU process is the harvester's own work (only while it runs: the
// same WSL VM also runs the human's containers).
func (d *Daemon) harvesterProc(name string) bool {
	if !d.harvest.on {
		return false
	}
	for _, p := range d.cfg.Harvester.Processes {
		if sysinfo.NameIs(strings.ToLower(strings.TrimSuffix(name, ".exe")), strings.TrimSuffix(p, ".exe")) {
			return true
		}
	}
	return false
}

func (d *Daemon) harvester(now time.Time) {
	h := d.cfg.Harvester
	if h.Start == "" {
		return
	}
	d.harvestBooks(now)
	busy := d.gpuHeld()
	for _, j := range d.jobs {
		busy = busy || (j.GPU && j.State != "done")
	}
	if !d.harvest.on && len(d.outSince) > 0 {
		busy = true // heavy GPU work outside firefighter (a render, a local model) is the human's
	}
	why := "GPU wanted"
	if h.Profit.On() && !d.harvest.check.OK {
		busy, why = true, "not paying: "+d.harvest.check.Why
	}
	if busy {
		d.harvest.quietFrom = time.Time{}
		if d.harvest.on {
			d.harvest.on = false
			logf("harvester: stop (%s)", why)
			go shell(h.Stop)
		}
		d.harvestSelfStarted(now)
		return
	}
	if d.harvest.quietFrom.IsZero() {
		d.harvest.quietFrom = now
		d.harvest.cpuBase = d.snap.CPUBusy
	}
	if !d.harvest.on {
		d.harvest.cpuBase += 0.05 * (d.snap.CPUBusy - d.harvest.cpuBase)
	}
	if !d.harvest.on && now.Sub(d.harvest.quietFrom).Seconds() >= h.IdleSeconds {
		d.harvest.on, d.harvest.accAt = true, now
		if h.Profit.On() {
			logf("harvester: start (GPU idle %.0fs; %s)", h.IdleSeconds, d.harvest.check.Why)
		} else {
			logf("harvester: start (GPU idle %.0fs)", h.IdleSeconds)
		}
		go shell(h.Start)
		return
	}
	d.harvestSelfStarted(now)
}

// harvestSelfStarted stops a harvester that started itself while firefighter has it off (its own
// auto-start), so firefighter alone decides when it runs.
func (d *Daemon) harvestSelfStarted(now time.Time) {
	h := d.cfg.Harvester
	if d.harvest.on || h.Running == "" || now.Sub(d.harvest.runAt) < 2*time.Minute || !d.harvest.probing.CompareAndSwap(false, true) {
		return
	}
	d.harvest.runAt = now
	go func() {
		defer d.harvest.probing.Store(false)
		if shellOK(h.Running, 30*time.Second) {
			logf("harvester: it started on its own while off; stopping it")
			shell(h.Stop)
		}
	}()
}

// harvestBooks prices the harvester's power into the ledger while it runs and, once a minute,
// asks the profit guard whether it may run.
func (d *Daemon) harvestBooks(now time.Time) {
	p := d.cfg.Harvester.Profit
	if !p.On() {
		d.harvest.book = nil
		return
	}
	if d.harvest.book == nil {
		d.harvest.book = harvest.Open(config.Home())
	}
	b := d.harvest.book
	gpuW, limitW := 0.0, 0.0
	for _, g := range d.snap.GPUs {
		gpuW += g.Watts
		limitW += g.LimitW
		if g.Watts == 0 && g.LimitW > 0 { // no reading: assume its share of the limit
			gpuW += g.LimitW * g.Util / 100
		}
	}
	if d.harvest.on {
		cpus := math.Max(0, d.snap.CPUBusy-d.harvest.cpuBase)
		b.Accrue(now, now.Sub(d.harvest.accAt), harvest.WallWatts(p, gpuW, cpus), gpuW, p.Price(now))
		d.harvest.accAt = now
	}
	if now.Sub(d.harvest.checkAt) < time.Minute {
		return
	}
	d.harvest.checkAt = now
	if err := b.Save(); err != nil {
		logf("harvest ledger: %v", err)
	}
	b.LoadEarnings()
	if limitW == 0 {
		limitW = 300
	}
	prev := d.harvest.check
	c := b.Decide(p, now, harvest.WallWatts(p, limitW, 2))
	if trip, err := os.ReadFile(harvest.TripPath(config.Home())); err == nil {
		c.OK, c.Why = false, "tripped: "+strings.TrimSpace(string(trip))+" (delete harvest-trip.txt or run firefighter harvest reset to allow it again)"
	} else if c.Trip {
		_ = os.WriteFile(harvest.TripPath(config.Home()), []byte(now.Format("2006-01-02 15:04")+" "+c.Why+"\n"), 0o600)
		logf("harvester: tripped off: %s", c.Why)
	}
	d.harvest.check = c
	if prev.OK != c.OK || prev.Why == "" {
		logf("harvest guard: %s", c.Why)
	}
}

// shellOK runs cmd and says whether it exited 0 within limit.
func shellOK(cmd string, limit time.Duration) bool {
	argv := []string{"sh", "-c", cmd}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", cmd}
	}
	c := exec.Command(argv[0], argv[1:]...)
	sysinfo.PrepareBackground(c)
	if err := c.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(limit):
		_ = c.Process.Kill()
		return false
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

// refreshDisks reads the free space of every drive in use (temp files, firefighter's state, each
// open job's folder) in the background every 5 s.
func (d *Daemon) refreshDisks(now time.Time, folders []string) {
	if d.diskBusy.Load() || now.Sub(d.diskAt) < 5*time.Second {
		return
	}
	d.diskBusy.Store(true)
	d.diskAt = now
	go func() {
		defer d.diskBusy.Store(false)
		disks, driveOf, sys := map[string]sysinfo.Disk{}, map[string]string{}, map[string]bool{}
		for i, f := range folders {
			if _, ok := driveOf[f]; ok {
				continue
			}
			if dk, ok := sysinfo.DiskOf(f); ok {
				disks[dk.Path], driveOf[f] = dk, dk.Path
				if i < 2 {
					sys[dk.Path] = true // the first two are the temp folder and firefighter's home
				}
			}
		}
		d.mu.Lock()
		d.disks, d.driveOf, d.sysDrives = disks, driveOf, sys
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

// ramSettling says why no job may start for RAM reasons right now ("" when none): a reclaim is
// running, a job is suspended to free RAM (it comes back first), or RAM was under the reserve
// within the last reserve.ram_shrink_seconds.
func (d *Daemon) ramSettling(now time.Time) string {
	if d.reclaim.on {
		return "giving the WSL/Docker VM's file cache back to Windows first"
	}
	for _, j := range d.jobs {
		if j.State == "running" && j.held == "ram" {
			return j.Name + " is suspended to free RAM and resumes first"
		}
	}
	if ago := now.Sub(d.ramLowLast).Seconds(); !d.ramLowLast.IsZero() && ago < d.cfg.Reserve.RAMShrinkSeconds {
		if ago < 1 {
			return fmt.Sprintf("RAM is under the human's %.0f%% reserve (%.1f GB free)", d.res.RAMPct, d.snap.RAMAvailGB)
		}
		return fmt.Sprintf("RAM was under the human's reserve %.0f s ago: waiting %.0f s for it to settle", ago, d.cfg.Reserve.RAMShrinkSeconds)
	}
	return ""
}

// heldForHuman: the human's state would hold this job anyway (a game holds the GPU or the network).
func (d *Daemon) heldForHuman(j *job) bool {
	return (j.GPU && d.gpuHeld()) || (j.Net && d.user == "game" && d.cfg.Network.PauseInGame)
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
		why := fmt.Sprintf("RAM below the human's %.0f%% reserve (%.1f GB free, %.1f GB short)", d.res.RAMPct, avail, need)
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
			v.reason = fmt.Sprintf("RAM critically low: %.1f GB free, %.1f GB short; stopping it frees %.1f GB", avail, need, v.ramGB)
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

// diskFloor is the free space kept on a drive: reserve.disk_free_gb, at most a tenth of the drive.
func (d *Daemon) diskFloor(dk sysinfo.Disk) float64 {
	return math.Min(d.cfg.Reserve.DiskFreeGB, dk.TotalGB/10)
}

// drivesOf lists the drives a job can fill: its folder's, and the ones temp files and caches go to.
func (d *Daemon) drivesOf(j *job) []string {
	var out []string
	if dr, ok := d.driveOf[j.Cwd]; ok {
		out = append(out, dr)
	}
	for dr := range d.sysDrives {
		if len(out) == 0 || out[0] != dr {
			out = append(out, dr)
		}
	}
	sort.Strings(out)
	return out
}

// diskWhy says which drive under the human's disk reserve keeps a job from starting ("" if none).
func (d *Daemon) diskWhy(j *job) string {
	if d.cfg.Reserve.DiskFreeGB <= 0 {
		return ""
	}
	for _, dr := range d.drivesOf(j) {
		if dk := d.disks[dr]; dk.TotalGB > 0 && dk.FreeGB < d.diskFloor(dk) {
			return fmt.Sprintf("disk %s has %.1f GB free, under the %.0f GB kept for the human", dr, dk.FreeGB, d.diskFloor(dk))
		}
		if dk := d.disks[dr]; dk.TotalGB > 0 && j.State == "waiting" && j.NeedDisk >= 1 && dr == d.driveOf[j.Cwd] && dk.FreeGB-d.diskFloor(dk) < j.NeedDisk {
			return fmt.Sprintf("usually writes %.1f GB; disk %s has %.1f GB beyond the %.0f GB kept for the human", j.NeedDisk, dr, dk.FreeGB-d.diskFloor(dk), d.diskFloor(dk))
		}
	}
	return ""
}

// cpuWhy says when a job waits for CPUs ("" otherwise): its past runs keep NeedCPU CPUs busy, and
// with the jobs already running that passes what's outside the human's reserve (cpuRoom). A job
// with no history claims nothing, and nothing waits while no other job runs.
func (d *Daemon) cpuWhy(j *job) string {
	used := d.jobCPUs()
	if j.State == "running" || j.State == "granted" {
		used -= math.Max(j.cpuNow, j.NeedCPU) // not itself
	}
	if j.NeedCPU < 1 || used < 0.5 || used+j.NeedCPU <= d.cpuRoom() {
		return ""
	}
	return fmt.Sprintf("usually keeps %.1f CPUs busy; jobs already running use %.1f of the %.0f outside the human's reserve", j.NeedCPU, used, d.cpuRoom()/1.25)
}

// diskPressure keeps free space on every drive in use. A drive under the reserve for
// pressure_seconds: the job writing the most to it is suspended (stopping it would free nothing,
// and its files stay), one job at a time, and resumes when the drive has room again. No job starts
// on a drive that is under the reserve (admit).
func (d *Daemon) diskPressure(now time.Time) {
	c := d.cfg
	if d.diskLow == nil {
		d.diskLow = map[string]time.Time{}
	}
	for dr := range d.diskLow {
		if _, ok := d.disks[dr]; !ok {
			delete(d.diskLow, dr)
		}
	}
	for dr, dk := range d.disks {
		floor := d.diskFloor(dk)
		low := c.Reserve.DiskFreeGB > 0 && dk.FreeGB < floor
		d.diskLow[dr] = since(d.diskLow[dr], low, now)
		if !low || now.Sub(d.diskLow[dr]) <= dur(c.Ladder.PressureSeconds) || now.Sub(d.lastHold) <= holdGap {
			continue
		}
		var v *job
		for _, j := range d.running() {
			if j.held != "" || !j.suspended.IsZero() || !j.killedAt.IsZero() || !j.checkpoint.IsZero() || j.writeRate < 1 {
				continue
			}
			on := false
			for _, jd := range d.drivesOf(j) {
				on = on || jd == dr
			}
			if on && (v == nil || j.writeRate > v.writeRate) {
				v = j
			}
		}
		if v != nil {
			v.heldDrive = dr
			d.hold(v, "disk", fmt.Sprintf("disk %s has %.1f GB free, under the %.0f GB kept for the human, and it was writing %.0f MB/s", dr, dk.FreeGB, floor, v.writeRate), now)
			return
		}
	}
	for _, j := range d.running() {
		if j.held != "disk" || !j.checkpoint.IsZero() {
			continue
		}
		dk, known := d.disks[j.heldDrive]
		floor := d.diskFloor(dk)
		ok := c.Reserve.DiskFreeGB <= 0 || !known || dk.FreeGB >= floor+math.Max(1, floor/4)
		j.okSince = since(j.okSince, ok, now)
		if ok && now.Sub(j.okSince) >= 5*time.Second && now.Sub(d.lastHold) > holdGap {
			d.unhold(j, now, fmt.Sprintf("disk %s has room again (%.1f GB free)", j.heldDrive, dk.FreeGB))
			return
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
