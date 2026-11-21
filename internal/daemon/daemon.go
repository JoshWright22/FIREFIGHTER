// Package daemon is the guard: one per user per machine. It samples the machine twice a second,
// sizes the human's reserve, admits waiting jobs in policy order, charges budgets, and walks the
// preemption ladder when the human needs resources back.
package daemon

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/harvest"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

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
	gameApp       string    // the app a fullscreen game was detected in: game holds while it stays in front
	userSamples   []string  // the last raw readings of the human's state, 10 s apart (hysteresis)
	userSampleAt  time.Time // when the last one was taken
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
	ramStuckLog   time.Time  // last "short of RAM, but no job's RAM would cover it" log line
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
	storeBusy     atomic.Bool
	storeAt       time.Time
	storeRep      *storage.Report // agents' storage spaces, measured every 2 minutes
	humanAt       time.Time       // last sample of the human's use (every 5 s)
	ramRise       usage.Growth    // the human's RAM over the last 5 minutes: how far it rises
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
	seenCtr       map[string]bool // containers already looked at for an owner job
	notify        chan struct{}
	rng           *rand.Rand
	admissionDraw []*policy.Ticket
	harvestQ      chan harvestCmd // harvester start/stop commands, run in order, one at a time
	harvest       struct {
		on        bool
		stop      string // the stop command of the harvester that was started (config can change while it runs)
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
	updating   atomic.Bool // about to restart as a newer version: no new grants
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
	// Harvester start and stop run one at a time, in the order asked: a slow start can't finish
	// after the stop that followed it and leave the harvester running (review 11/20).
	d.harvestQ = make(chan harvestCmd, 64)
	go func() {
		for c := range d.harvestQ {
			shellWait(c.cmd, 90*time.Second) // to completion (90 s at most): a start can't finish after the stop behind it, and a hung one can't hold the stop for long
			if c.done != nil {
				close(c.done)
			}
		}
	}()
	// The harvester's state isn't known after a restart: stop it; it restarts after the idle wait.
	d.harvestDo(d.cfg.Harvester.Stop)
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
