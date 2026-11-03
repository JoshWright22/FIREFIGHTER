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
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

const tick = 500 * time.Millisecond

type job struct {
	*policy.Ticket
	pids       map[int]bool
	applied    map[int]string // pid -> cpu set applied
	cpuPrev    float64
	cpuNow     float64 // CPUs in use
	ramGB      float64
	vramGB     float64
	gpuUtil    float64
	peakVRAM   float64
	peakRAM    float64
	charged    float64
	overrun    bool
	granted    time.Time
	pausedAt   time.Time // cooperative pause flag set
	suspended  time.Time
	checkpoint time.Time // checkpoint-and-exit requested
	reason     string
	container  string
	receipt    *Receipt
	extended   float64
	price      float64 // locked at grant: bid price x congestion x surcharge
	adopted    bool    // auto-adopted: heavy work an agent started outside firefighter
	termAt     time.Time
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
	scanner   *sysinfo.Scanner
	rogueAt   time.Time
	rogueSeen map[int]time.Time
	saveAt    time.Time
	statusAt  time.Time
	started   time.Time
	Token     string
	Port      int
}

func New() *Daemon {
	d := &Daemon{
		jobs:    map[string]*job{},
		reader:  sysinfo.NewReader(),
		st:      load(),
		notify:  make(chan struct{}),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		started: time.Now(),
		user:    "present",
	}
	d.reloadConfig()
	_ = os.MkdirAll(signalDir(), 0o755)
	return d
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

// Run is the control loop. It never returns.
func (d *Daemon) Run() {
	t := time.NewTicker(tick)
	last := time.Now()
	for now := range t.C {
		snap := d.reader.Read()
		var others []sysinfo.Container
		dockerDue := now.Sub(d.statusAt) > 14*time.Second
		if dockerDue {
			others = sysinfo.Containers()
		}
		d.mu.Lock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					logf("loop panic: %v", r)
				}
			}()
			if dockerDue {
				d.others = others
			}
			d.step(snap, now, now.Sub(last))
		}()
		d.mu.Unlock()
		last = now
	}
}

func (d *Daemon) step(snap sysinfo.Snapshot, now time.Time, dt time.Duration) {
	d.reloadConfig()
	c := d.cfg
	d.snap = snap
	d.measureJobs(now, dt)
	d.observeOccupancy(now, dt)
	d.senseUser(now)
	d.adoptRogues(now)
	d.res.Update(c, d.user, d.busyUser, now)
	d.enforceCPU()
	d.ladder(now)
	d.reap(now)
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

func (d *Daemon) measureJobs(now time.Time, dt time.Duration) {
	c := d.cfg
	capy := d.capacity()
	jobPIDs := map[int]bool{}
	var gpuJobs []*job
	for _, j := range d.running() {
		if j.PID > 0 {
			for _, p := range sysinfo.Tree(j.PID) {
				if !j.pids[p] {
					j.pids[p] = true
					if err := sysinfo.Background(p); err != nil && p == j.PID {
						logf("%s: background priority: %v", j.Name, err)
					}
				}
			}
			for p := range j.pids {
				if p != j.PID && !sysinfo.Alive(p) {
					delete(j.pids, p)
					delete(j.applied, p)
				}
			}
		}
		var pids []int
		for p := range j.pids {
			pids = append(pids, p)
			jobPIDs[p] = true
		}
		cpuSec, ram := sysinfo.Usage(pids)
		if j.cpuPrev > 0 && dt > 0 {
			j.cpuNow = math.Max(0, (cpuSec-j.cpuPrev)/dt.Seconds())
		}
		j.cpuPrev = cpuSec
		j.ramGB = ram
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
	// Charge (DRF-style machine-minutes, priced by the bid, idle VRAM taxed, overruns doubled).
	m := dt.Minutes()
	for _, j := range d.running() {
		if !j.suspended.IsZero() || m <= 0 || m > 0.1 {
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
		if jobPIDs[pid] || g.Util < 1 {
			continue
		}
		n := sysinfo.Name(pid)
		if n == "" || contains(c.Reserve.IgnoreGPUApps, n) {
			continue
		}
		d.foreign[n] = math.Max(d.foreign[n], g.Util)
		if g.Util3D >= 1 {
			d.graphics[n] = math.Max(d.graphics[n], g.Util3D)
		}
	}
}

func (d *Daemon) capacity() policy.Capacity {
	vt, _ := d.snap.VRAMTotal()
	return policy.Capacity{CPUs: float64(max(1, d.snap.CPUs)), RAMGB: math.Max(1, d.snap.RAMTotalGB), VRAMGB: vt}
}

func (d *Daemon) account(project string, now time.Time) *policy.Account {
	a := d.st.Accounts[project]
	if a == nil {
		a = &policy.Account{}
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
	for n, u := range d.graphics { // 3D engine only: CUDA compute outside firefighter is load, not play
		if u >= c.Reserve.ForeignGPUPct {
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
		for p := range j.pids {
			if j.applied[p] == key {
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

func (d *Daemon) ladder(now time.Time) {
	c := d.cfg
	r := c.ReserveFor(d.user)
	dur := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

	// Overrun: past the lease x factor without an extension -> spot band, double charge.
	for _, j := range d.running() {
		lease := (j.Minutes + j.extended) * c.Ladder.OverrunFactor
		if j.Minutes > 0 && !j.overrun && now.Sub(j.Started).Minutes() > lease {
			j.overrun, j.Band = true, policy.BandSpot
			logf("%s: past its %.0f-minute lease; now spot (charged double, preempted first)", j.Name, j.Minutes+j.extended)
		}
	}

	// Game or media in front: cooperative pause -> suspend (bounded) -> checkpoint for GPU jobs.
	gpuHold := r.PauseGPU && d.user == "game"
	for _, j := range d.running() {
		if !j.GPU {
			continue
		}
		switch {
		case gpuHold && j.pausedAt.IsZero():
			j.pausedAt, j.reason = now, "you're gaming ("+d.gameWhy+")"
			touch(PauseFile(j.ID))
			logf("%s: pause requested: %s", j.Name, j.reason)
		case gpuHold && j.suspended.IsZero() && j.checkpoint.IsZero() && !exists(PausedFile(j.ID)) &&
			now.Sub(j.pausedAt) > dur(c.Ladder.PauseAckSeconds):
			j.suspended = now
			d.each(j, sysinfo.Suspend)
			d.signalContainer(j, "pause")
			logf("%s: no pause ack in %.0fs, suspended", j.Name, c.Ladder.PauseAckSeconds)
		case gpuHold && !j.suspended.IsZero() && j.checkpoint.IsZero() && j.Evict != "never" &&
			now.Sub(j.suspended) > dur(c.Ladder.SuspendMaxSeconds):
			d.each(j, sysinfo.Resume)
			j.suspended = time.Time{}
			d.requestCheckpoint(j, now, "game still running after the suspend limit")
		case !gpuHold && !j.pausedAt.IsZero():
			_ = os.Remove(PauseFile(j.ID))
			d.signalContainer(j, "unpause")
			if !j.suspended.IsZero() {
				d.each(j, sysinfo.Resume)
			}
			j.pausedAt, j.suspended, j.reason = time.Time{}, time.Time{}, ""
			logf("%s: resumed", j.Name)
		}
	}

	// RAM and VRAM pressure: under the reserve for PressureSeconds -> checkpoint one victim.
	ramFloor := d.snap.RAMTotalGB * d.res.RAMPct / 100
	ramShort := d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < ramFloor
	vt, vu := d.snap.VRAMTotal()
	vramShort := vt > 0 && vt-vu < d.res.VRAMGB
	d.ramLow = since(d.ramLow, ramShort, now)
	d.vramLow = since(d.vramLow, vramShort, now)
	if !d.ramLow.IsZero() && now.Sub(d.ramLow) > dur(c.Ladder.PressureSeconds) {
		if v := d.victim(false, func(j *job) float64 { return j.ramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("RAM below your %.0f%% reserve", d.res.RAMPct))
		}
	}
	if !d.vramLow.IsZero() && now.Sub(d.vramLow) > dur(c.Ladder.PressureSeconds) {
		if v := d.victim(true, func(j *job) float64 { return j.vramGB }); v != nil {
			d.requestCheckpoint(v, now, fmt.Sprintf("VRAM below your %.1f GB reserve", d.res.VRAMGB))
		}
	}
	// Critical RAM: kill at once (lmkd/earlyoom ordering: highest band, heaviest first).
	if d.snap.RAMTotalGB > 0 && d.snap.RAMAvailGB < d.snap.RAMTotalGB*c.Ladder.KillBelowRAMPct/100 {
		if v := d.victim(false, func(j *job) float64 { return j.ramGB }); v != nil {
			v.reason = "RAM critically low"
			d.killJob(v)
		}
	}

	// A spot job holding the GPU gives way when an in-budget GPU job is waiting.
	waitingGPU := false
	for _, j := range d.jobs {
		waitingGPU = waitingGPU || (j.State == "waiting" && j.GPU && j.Band < policy.BandSpot)
	}
	if waitingGPU {
		for _, j := range d.running() {
			if j.GPU && j.Band == policy.BandSpot && j.checkpoint.IsZero() && j.Evict != "never" {
				d.requestCheckpoint(j, now, "spot job yields to an in-budget job")
				break
			}
		}
	}

	// Checkpoint grace over: SIGTERM where there is one (10 s to exit), then kill.
	for _, j := range d.running() {
		if j.checkpoint.IsZero() || now.Sub(j.checkpoint) <= dur(c.Ladder.GraceSeconds) {
			continue
		}
		if runtime.GOOS != "windows" && j.termAt.IsZero() && j.PID > 0 {
			j.termAt = now
			_ = sysinfo.Terminate(j.PID)
			logf("%s: grace over, SIGTERM", j.Name)
		} else if runtime.GOOS == "windows" || now.Sub(j.termAt) > 10*time.Second {
			d.killJob(j)
		}
	}
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

// victim picks the job to evict: one already being evicted counts (one at a time), else highest
// band, never-evict jobs last, heaviest first.
func (d *Daemon) victim(gpuOnly bool, weight func(*job) float64) *job {
	var best *job
	for _, j := range d.running() {
		if !j.checkpoint.IsZero() {
			return nil
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

func (d *Daemon) requestCheckpoint(j *job, now time.Time, why string) {
	if !j.checkpoint.IsZero() {
		return
	}
	j.checkpoint, j.reason = now, why
	touch(CheckpointFile(j.ID))
	if j.Evict == "force" {
		d.killJob(j)
		return
	}
	logf("%s: checkpoint and exit requested (%s); %.0fs grace", j.Name, why, d.cfg.Ladder.GraceSeconds)
	d.broadcast()
}

func (d *Daemon) killJob(j *job) {
	logf("%s: killed (%s)", j.Name, j.reason)
	if j.container != "" {
		go docker("kill", j.container)
	}
	for p := range j.pids {
		_ = sysinfo.Kill(p)
	}
	j.checkpoint = time.Now().Add(-24 * time.Hour) // don't re-kill every tick
}

func (d *Daemon) each(j *job, f func(int) error) {
	for p := range j.pids {
		_ = f(p)
	}
}

func (d *Daemon) signalContainer(j *job, verb string) {
	if j.container != "" {
		go docker(verb, j.container)
	}
}

// reap closes tickets whose client and job are both gone.
func (d *Daemon) reap(now time.Time) {
	for id, j := range d.jobs {
		if j.State == "done" {
			continue
		}
		clientGone := j.ClientPID > 0 && !sysinfo.Alive(j.ClientPID)
		jobGone := j.PID <= 0 || !sysinfo.Alive(j.PID)
		if j.State == "granted" && clientGone {
			d.finish(id, -1, "lost", "")
		} else if j.State == "waiting" && clientGone {
			d.finish(id, -1, "cancelled", "")
		} else if j.State == "running" && clientGone && jobGone {
			outcome := "lost"
			if j.adopted {
				outcome = "ended"
			}
			d.finish(id, -1, outcome, "")
		} else if j.State == "granted" && now.Sub(j.granted) > 2*time.Minute {
			j.State = "waiting"
		}
	}
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
// jobs haven't grown into yet.
func (d *Daemon) headroom(now time.Time) (ram, vram float64) {
	ram = d.snap.RAMAvailGB - d.snap.RAMTotalGB*d.res.RAMPct/100
	vt, vu := d.snap.VRAMTotal()
	vram = vt - vu - d.res.VRAMGB
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
	vt, _ := d.snap.VRAMTotal()
	gpuRunning := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") {
			gpuRunning++
		}
	}
	// GPU compute outside firefighter (another agent's unmanaged job, a local model server) holds a slot.
	for _, u := range d.foreign {
		if u >= c.Reserve.ForeignGPUPct {
			gpuRunning++
		}
	}
	gpuBlocked := c.ReserveFor(d.user).PauseGPU && d.user == "game"
	if gpuRunning > 0 && d.snap.GPUUtil() >= c.GPU.PackBelowUtil {
		gpuBlocked = true
	}
	changed := false
	headWaitsOnVRAM := false
	for _, t := range d.waiting() {
		j := d.jobs[t.ID]
		if t.GPU {
			if gpuBlocked || gpuRunning >= c.GPU.MaxJobs {
				continue
			}
			if vt > 0 && t.NeedVRAM > vram {
				if !headWaitsOnVRAM {
					headWaitsOnVRAM = true // the head waits for VRAM; only short jobs may backfill
				}
				continue
			}
			// EASY-style backfill: behind a head that waits on VRAM, only a short job that fits
			// now may start, so the head is delayed by at most ~10 minutes.
			if headWaitsOnVRAM && t.PredMinutes > 10 {
				continue
			}
		}
		if t.NeedRAM > ram {
			if t.GPU {
				gpuBlocked = true
			}
			continue
		}
		ram -= t.NeedRAM
		if t.GPU {
			vram -= t.NeedVRAM
			gpuRunning++
		}
		j.State, j.granted = "granted", now
		d.lockPrice(j, now)
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
	busy := d.user == "game"
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
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	sysinfo.Detach(c, true)
	if err := c.Start(); err != nil {
		logf("shell %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

func docker(args ...string) {
	c := exec.Command("docker", args...)
	sysinfo.Detach(c, false)
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
	f, err := os.OpenFile(filepath.Join(config.Home(), "daemon.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
