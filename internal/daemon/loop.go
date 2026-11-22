package daemon

// Control loop: sampling, configuration reloads, housekeeping, and notifications.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

const tick = 500 * time.Millisecond

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
		if cfg.Storage.GB > 0 { // agents' storage spaces: jobs write there, so its drive's reserve counts for every job
			folders = append(folders, cfg.StorageRoot())
		}
		sysFolders := len(folders)
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
		d.refreshDisks(now, folders, sysFolders)
		d.refreshStorage(now, cfg)

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
	d.res.Update(d.withLearnedReserve(c, now), d.user, d.humanCPULoad(), d.snap.CPUs, now)
	d.enforceCPU()
	d.ladder(now)
	d.reap(now)
	d.refreshBands(now)
	d.refreshBackground(now)
	d.admit(now)
	d.sampleUtil(now, dt.Seconds())
	d.harvester(now)
	if now.Sub(d.saveAt) > 10*time.Second {
		d.saveAt = now
		d.prune(now)
		d.st.Open = d.openJobs(now)
		d.st.Queued = d.queuedJobs(now)
		if err := d.st.write(); err != nil {
			logf("save: %v", err)
		}
	}
	if now.Sub(d.statusAt) > 15*time.Second {
		d.statusAt = now
		d.writeStatus()
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
