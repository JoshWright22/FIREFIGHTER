package daemon

// Idle harvester scheduling, commands, and bookkeeping.

import (
	"context"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/harvest"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

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
		// Unconfigured while it ran: stop it with the command it was started with (review 11/20:
		// it ran on, even when a job later wanted the GPU).
		if d.harvest.on {
			d.harvest.on = false
			logf("harvester: stop (no longer configured)")
			if d.harvest.stop != "" {
				d.harvestDo(d.harvest.stop)
			}
		}
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
			stop := d.harvest.stop
			if stop == "" {
				stop = h.Stop
			}
			d.harvestDo(stop)
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
		d.harvest.on, d.harvest.accAt, d.harvest.stop = true, now, h.Stop
		if h.Profit.On() {
			logf("harvester: start (GPU idle %.0fs; %s)", h.IdleSeconds, d.harvest.check.Why)
		} else {
			logf("harvester: start (GPU idle %.0fs)", h.IdleSeconds)
		}
		d.harvestDo(h.Start)
		return
	}
	d.harvestSelfStarted(now)
}

// harvestDo queues a harvester start or stop command (run in order, one at a time).
func (d *Daemon) harvestDo(cmd string) {
	if cmd == "" {
		return
	}
	select {
	case d.harvestQ <- harvestCmd{cmd: cmd}:
	case <-time.After(time.Second):
		logf("harvester: %d commands already queued; dropped %q", cap(d.harvestQ), cmd) // never run out of order
	}
}

// harvestCmd is one queued harvester command; done (if set) is closed once it has finished.
type harvestCmd struct {
	cmd  string
	done chan struct{}
}

// harvestDoWait queues cmd behind whatever is queued and waits until it has run (at most limit):
// shutdown's stop can't overtake a start still running.
func (d *Daemon) harvestDoWait(cmd string, limit time.Duration) {
	if cmd == "" || d.harvestQ == nil {
		return
	}
	done := make(chan struct{})
	select {
	case d.harvestQ <- harvestCmd{cmd: cmd, done: done}:
	case <-time.After(limit):
		return
	}
	select {
	case <-done:
	case <-time.After(limit):
	}
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
		if !harvest.Approved(config.Home(), h.Running) {
			d.notApproved(h.Running)
			return
		}
		if shellOK(h.Running, 30*time.Second) {
			logf("harvester: it started on its own while off; stopping it")
			d.harvestDo(h.Stop) // in order with the loop's starts and stops
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

// notApproved logs, once per command, a harvester command that isn't approved (and isn't run).
func (d *Daemon) notApproved(cmd string) {
	d.harvestRefused.Do(cmd, func() {
		logf("harvester: not running %q: it isn't approved. Check it, then run `firefighter harvest approve` from your own terminal", cmd)
	})
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

// shellWait runs cmd and waits for it to finish (at most limit).
func shellWait(cmd string, limit time.Duration) {
	if cmd == "" {
		return
	}
	argv := []string{"sh", "-c", cmd}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", cmd}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	sysinfo.PrepareBackground(c)
	if err := c.Run(); err != nil && ctx.Err() != nil {
		logf("shell %q: still running after %s; moved on", cmd, limit)
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

// onceSet runs f once per key (for log lines that would repeat every tick).
type onceSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (o *onceSet) Do(key string, f func()) {
	o.mu.Lock()
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	first := !o.seen[key]
	o.seen[key] = true
	o.mu.Unlock()
	if first {
		f()
	}
}
