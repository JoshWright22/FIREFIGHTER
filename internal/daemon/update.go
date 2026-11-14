package daemon

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/update"
)

// autoUpdate keeps a released guard current: it looks for a newer release a few minutes after it
// starts and every 6 hours, installs it at a moment when no job is running or about to (waiting
// jobs get back in line by themselves), and restarts as the new version. A development build
// ("dev") never updates itself, and update.auto = false turns it off.
func (d *Daemon) autoUpdate() {
	if d.Version == "dev" || d.Version == "" || d.Exe == "" {
		return
	}
	update.Cleanup(d.Exe)
	first, every, poll := 3*time.Minute, 6*time.Hour, 30*time.Second
	if s, err := strconv.ParseFloat(os.Getenv("FIREFIGHTER_UPDATE_EVERY"), 64); err == nil && s > 0 && !config.IsDefaultHome() {
		first, every, poll = dur(s), dur(s), dur(s) // tests, in their own home
	}
	t := time.NewTimer(first)
	defer t.Stop()
	said := ""
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
		}
		t.Reset(every)
		if !d.cfgShared.Load().Update.Auto {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rel, err := update.Latest(ctx)
		cancel()
		if err != nil || !update.Newer(rel.Tag, d.Version) {
			continue // offline, rate-limited or current: look again next time
		}
		if said != rel.Tag {
			said = rel.Tag
			logf("update: %s is out (this is %s); it installs when no job is running", rel.Tag, d.Version)
		}
		d.setUpdateNote("firefighter " + rel.Tag + " is out: it installs itself when no job is running")
		for !d.quietForUpdate() {
			select {
			case <-d.stop:
				return
			case <-time.After(poll):
			}
		}
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Minute)
		err = update.Apply(ctx, rel, d.Exe, func(s string) { logf("update: %s", s) })
		cancel()
		if err != nil {
			logf("update to %s failed: %v (trying again in %s)", rel.Tag, err, every)
			d.setUpdateNote("updating to " + rel.Tag + " failed: " + err.Error())
			continue
		}
		logf("update: restarting as %s", rel.Tag)
		d.restart.Store(true)
		d.Shutdown()
		return
	}
}

func (d *Daemon) setUpdateNote(s string) {
	d.mu.Lock()
	d.updateNote = s
	d.mu.Unlock()
}

// quietForUpdate: no job is running or has just been given its turn. (A restart would not lose
// them: their clients re-register. But a job mid-run is not the moment to swap the guard.)
func (d *Daemon) quietForUpdate() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State == "running" || j.State == "granted" {
			return false
		}
	}
	return true
}

// Restarting says whether the guard stopped in order to come back as a newer version.
func (d *Daemon) Restarting() bool { return d.restart.Load() }
