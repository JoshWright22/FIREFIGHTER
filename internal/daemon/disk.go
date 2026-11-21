package daemon

// Disk reserve, storage claims, and disk pressure.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// refreshDisks reads the free space of every drive in use (temp files, firefighter's state, each
// open job's folder) in the background every 5 s.
func (d *Daemon) refreshDisks(now time.Time, folders []string, sysFolders int) {
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
				if i < sysFolders {
					sys[dk.Path] = true // the temp folder, firefighter's home, the agents' storage root
				}
			}
		}
		d.mu.Lock()
		d.disks, d.driveOf, d.sysDrives = disks, driveOf, sys
		d.mu.Unlock()
	}()
}

// refreshStorage measures the agents' storage spaces in the background.
func (d *Daemon) refreshStorage(now time.Time, cfg config.Config) {
	if cfg.Storage.GB <= 0 {
		d.mu.Lock()
		d.storeRep = nil
		d.mu.Unlock()
		return
	}
	// Every 2 minutes, and at once after a claim or release changed storage.json.
	changed := false
	if fi, err := os.Stat(filepath.Join(config.Home(), "storage.json")); err == nil && fi.ModTime().After(d.storeAt) {
		changed = true
	}
	if d.storeBusy.Load() || (now.Sub(d.storeAt) < 2*time.Minute && !changed) {
		return
	}
	d.storeBusy.Store(true)
	d.storeAt = now
	go func() {
		defer d.storeBusy.Store(false)
		r, err := storage.FromConfig(cfg).Look(config.Home())
		if err != nil {
			return
		}
		d.mu.Lock()
		d.storeRep = &r
		d.mu.Unlock()
	}()
}

// storageWhy keeps a project's new jobs waiting while its storage space holds more than it claimed
// ("" otherwise). No exception for --quick: a quick job's lease can be overrun, and cleaning up
// (`firefighter storage free`, deleting files) needs no job (review 11/20).
func (d *Daemon) storageWhy(j *job) string {
	if d.storeRep == nil || j.State != "waiting" {
		return ""
	}
	for _, s := range d.storeRep.Spaces {
		if s.Over && (strings.EqualFold(s.Project, j.Project) || strings.EqualFold(s.Project, storage.Name(j.Project))) {
			if s.GB == 0 {
				return fmt.Sprintf("storage: %s holds %.1f GB in %s without a claim; `firefighter storage claim --gb N` or delete it", s.Project, s.UsedGB, s.Path)
			}
			return fmt.Sprintf("storage: %s's space holds %.1f GB, over its %.1f GB claim (%.1f GB of it ranked scratch); `firefighter storage free --gb %.1f` deletes its least important items, or `firefighter storage claim --gb %.0f`",
				s.Project, s.UsedGB, s.GB, s.RankGB[storage.Scratch], math.Ceil((s.UsedGB-s.GB)*10+0.5)/10, math.Ceil(s.UsedGB+1))
		}
	}
	return ""
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

// diskWhy says which drive under the human's disk reserve, or which storage space over its claim,
// keeps a job from starting ("" if none).
func (d *Daemon) diskWhy(j *job) string {
	if w := d.storageWhy(j); w != "" {
		return w
	}
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
