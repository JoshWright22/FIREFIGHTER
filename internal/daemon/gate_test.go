package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

func TestAdmissionGateAgreesWithBlockerAndETA(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      BlockKind
		text      string
		wantETA   float64
		configure func(*Daemon, *job, time.Time)
	}{
		{"updating", blockUpdating, "restart for an update", -1, func(d *Daemon, j *job, _ time.Time) {
			j.GPU = false
			d.updating.Store(true)
		}},
		{"CPU first-run pacing", blockPacing, "first-time commands start 5 s apart", 4.0 / 60, func(d *Daemon, j *job, now time.Time) {
			j.GPU = false
			delete(d.st.History, histKey(j.Ticket))
			d.unknownAt = map[bool]time.Time{false: now.Add(-time.Second)}
		}},
		{"network turn", blockNetwork, "network turn: another download has it", -1, func(d *Daemon, j *job, now time.Time) {
			j.GPU, j.Net = false, true
			d.cfg.Network.MaxJobs = 1
			d.addRunning("download", 1, now.Add(-time.Minute)).Net = true
		}},
		{"human bandwidth", blockNetwork, "the human is using 70 of ~100 Mbps", -1, func(d *Daemon, j *job, _ time.Time) {
			j.GPU, j.Net = false, true
			d.netCap, d.netHuman = 100, 70
			d.cfg.Network.HumanBusyPct = 50
		}},
		{"back-off", blockBackoff, "tries again in", 3, func(d *Daemon, j *job, now time.Time) {
			d.backoff = map[string]backoff{j.ID: {n: 3, at: now.Add(-time.Minute), why: "RAM"}}
		}},
		{"human gaming", blockHuman, "the human needs the GPU", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.user = "game"
		}},
		{"wait for human to leave", blockHuman, "starts while the human is away", -1, func(d *Daemon, j *job, _ time.Time) {
			j.NeedVRAM = 16
		}},
		{"partial GPU readings", blockGPUReading, "only some GPUs are reporting", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.gpuDegraded = true
		}},
		{"missing vendor", blockGPUVendor, "waiting for a amd GPU", -1, func(_ *Daemon, j *job, _ time.Time) {
			j.WantVendor = "amd"
		}},
		{"per-card slot limit", blockGPUSlot, "the selected card's slots are full", 0, func(d *Daemon, _ *job, now time.Time) {
			d.cfg.GPU.MaxJobs = 1
			d.snap.GPUs = append(d.snap.GPUs, sysinfo.GPU{Vendor: "amd", VRAMGB: 16})
			d.addRunning("owns the NVIDIA turn", 1, now.Add(-time.Minute))
		}},
		{"card above packing limit", blockGPUPacking, "at or above gpu.pack_below_util", 0, func(d *Daemon, _ *job, now time.Time) {
			d.cfg.GPU.MaxJobs, d.cfg.GPU.PackBelowUtil = 2, 60
			d.cardUtil[0] = 60
			d.addRunning("keeps card busy", 1, now.Add(-time.Minute))
		}},
		{"predicted packing load", blockGPUPacking, "usual load would exceed the card's remaining capacity", 0, func(d *Daemon, j *job, now time.Time) {
			d.cfg.GPU.MaxJobs, d.cfg.GPU.PackBelowUtil = 2, 60
			d.cardUtil[0], j.PredGPU = 40, 71
			d.addRunning("some card load", 1, now.Add(-time.Minute))
		}},
		{"outside GPU on a measured card", blockOutsideGPU, "GPU busy outside firefighter: outside 90%", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.cfg.GPU.MaxJobs = 1
			d.cardUtil[0], d.foreign = 90, map[string]float64{"outside": 90}
		}},
		{"outside GPU without card readings", blockOutsideGPU, "GPU busy outside firefighter: outside 90%", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.cfg.GPU.MaxJobs = 1
			d.snap.GPUs, d.foreign = nil, map[string]float64{"outside": 90}
		}},
		{"machine GPU slot without card readings", blockGPUSlot, "the GPU slots are full", 0, func(d *Daemon, _ *job, now time.Time) {
			d.cfg.GPU.MaxJobs = 1
			d.snap.GPUs = nil
			d.addRunning("owns the turn", 1, now.Add(-time.Minute))
		}},
		{"machine packing without card readings", blockGPUPacking, "utilization is at or above gpu.pack_below_util", 0, func(d *Daemon, _ *job, now time.Time) {
			d.snap.GPUs, d.utilEWMA = nil, d.cfg.GPU.PackBelowUtil
			d.addRunning("keeps GPU busy", 1, now.Add(-time.Minute))
		}},
		{"outside VRAM despite managed work on another card", blockVRAM, "needs 9.0 GB VRAM", -1, func(d *Daemon, j *job, now time.Time) {
			j.NeedVRAM = 9
			d.snap.GPUs[0].UsedGB = 12
			d.snap.GPUs = append(d.snap.GPUs, sysinfo.GPU{Vendor: "amd", VRAMGB: 16})
			other := d.addRunning("on a different card", 1, now.Add(-time.Minute))
			other.device, other.NeedVRAM, other.vramGB = 1, 8, 8
		}},
		{"VRAM held by a grant", blockVRAM, "needs 9.0 GB VRAM", 0, func(d *Daemon, j *job, now time.Time) {
			j.NeedVRAM = 9
			d.snap.GPUs[0].UsedGB = 5
			grant := d.addRunning("has a grant", 1, now.Add(-time.Minute))
			grant.State, grant.NeedVRAM = "granted", 5
		}},
		{"backfill behind outside VRAM", blockBackfill, "job waiting for VRAM on this card", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.snap.GPUs[0].UsedGB = 10
			d.addWaiting("VRAM head", 1, 30, time.Hour).NeedVRAM = 10
		}},
		{"backfill behind managed VRAM", blockBackfill, "job waiting for VRAM on this card", 0, func(d *Daemon, _ *job, now time.Time) {
			d.snap.GPUs[0].UsedGB = 10
			owner := d.addRunning("holds VRAM", 1, now.Add(-time.Minute))
			owner.vramGB, owner.NeedVRAM = 8, 8
			d.addWaiting("VRAM head", 1, 30, time.Hour).NeedVRAM = 10
		}},
		{"CPU head waiting for RAM", blockBackfill, "job waiting for RAM", -1, func(d *Daemon, j *job, _ time.Time) {
			j.GPU = false
			d.addWaiting("CPU RAM head", 25, 30, time.Hour).GPU = false
		}},
		{"GPU head waiting for RAM", blockBackfill, "job waiting for RAM", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.addWaiting("GPU RAM head", 25, 30, time.Hour)
		}},
		{"storage claim", blockStorage, "space holds 12.0 GB, over its 10.0 GB claim", -1, func(d *Daemon, j *job, _ time.Time) {
			d.storeRep = &storage.Report{Spaces: []storage.Space{{Project: j.Project, Over: true, GB: 10, UsedGB: 12}}}
		}},
		{"disk reserve", blockDisk, "under the 10 GB kept for the human", -1, func(d *Daemon, j *job, _ time.Time) {
			d.cfg.Reserve.DiskFreeGB = 10
			j.Cwd = "job dir"
			d.driveOf = map[string]string{j.Cwd: "drive"}
			d.disks = map[string]sysinfo.Disk{"drive": {TotalGB: 100, FreeGB: 8}}
		}},
		{"learned disk writes", blockDisk, "usually writes 5.0 GB", -1, func(d *Daemon, j *job, _ time.Time) {
			d.cfg.Reserve.DiskFreeGB = 10
			j.Cwd, j.NeedDisk = "job dir", 5
			d.driveOf = map[string]string{j.Cwd: "drive"}
			d.disks = map[string]sysinfo.Disk{"drive": {TotalGB: 100, FreeGB: 12}}
		}},
		{"managed CPUs", blockCPU, "usually keeps 8.0 CPUs busy", 0, func(d *Daemon, j *job, now time.Time) {
			j.GPU, j.NeedCPU = false, 8
			d.addRunning("uses CPUs", 1, now.Add(-time.Minute)).NeedCPU = 12
		}},
		{"outside RAM", blockRAM, "needs 2.0 GB RAM", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.snap.RAMAvailGB = 5
		}},
		{"managed RAM", blockRAM, "needs 2.0 GB RAM", 0, func(d *Daemon, _ *job, now time.Time) {
			d.snap.RAMAvailGB = 5
			d.addRunning("holds RAM", 3, now.Add(-time.Minute))
		}},
		{"RAM settling", blockRAM, "waiting 60 s for it to settle", 50.0 / 60, func(d *Daemon, _ *job, now time.Time) {
			d.cfg.Reserve.RAMShrinkSeconds = 60
			d.ramLowLast = now.Add(-10 * time.Second)
		}},
		{"RAM reclaim", blockRAM, "giving the WSL/Docker VM's file cache back", -1, func(d *Daemon, _ *job, _ time.Time) {
			d.reclaim.on = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDaemon(t)
			d.cfg.GPU.MaxJobs = 2
			j := d.addWaiting("candidate", 2, 30, time.Minute)
			now := time.Now()
			tc.configure(d, j, now)
			d.admit(now)
			if j.State != "waiting" {
				t.Fatalf("admit left job %s, want waiting", j.State)
			}
			decision := d.waitingDecision(j, now)
			if decision.Kind != tc.kind || decision.Forecastable != (tc.wantETA >= 0) {
				t.Fatalf("decision = %+v, want kind %v and forecastable %t", decision, tc.kind, tc.wantETA >= 0)
			}
			if why := d.blockerOf(j); !strings.Contains(why, tc.text) {
				t.Errorf("blocker = %q, want %q", why, tc.text)
			}
			if eta := d.startsIn(j, 0, now); eta != tc.wantETA {
				t.Errorf("ETA = %v, want %v", eta, tc.wantETA)
			}
			view := d.view(j)
			if !strings.Contains(view.Why, tc.text) || (view.StartsIn >= 0) != decision.Forecastable {
				t.Errorf("view gives %q with ETA %v, want %q and forecastable %t", view.Why, view.StartsIn, tc.text, decision.Forecastable)
			}
			offer, ok := view.Quote.Tier(view.Tier)
			if !ok || offer.StartsIn != view.StartsIn {
				t.Errorf("quote and view disagree: quote %+v, ETA %v", offer, view.StartsIn)
			}
			if tc.kind == blockBackoff && strings.Contains(view.Quote.Do, "run it now") {
				t.Errorf("quote advice ignores back-off: %q", view.Quote.Do)
			}
		})
	}
}

func TestAdmissionGateExceptionsRemainEligible(t *testing.T) {
	d := testDaemon(t)
	d.snap.RAMAvailGB = 5
	d.netCap, d.netHuman, d.cfg.Network.HumanBusyPct = 100, 100, 50
	j := d.addWaiting("overdue spot download", 2, 30, time.Hour)
	j.GPU, j.Net, j.Band = false, true, policy.BandSpot
	now := time.Now()
	if got := d.waitingDecision(j, now); got.Kind != blockNone || !got.Forecastable {
		t.Fatalf("overdue job alone needs no RAM margin and can bypass human bandwidth: %+v", got)
	}
	d.admit(now)
	if j.State != "granted" {
		t.Fatalf("eligible spot job not granted: %s", d.blockerOf(j))
	}
}

func TestGatePassAccountsForNewGrants(t *testing.T) {
	d := testDaemon(t)
	d.cfg.GPU.MaxJobs = 2
	d.utilEWMA = d.cfg.GPU.PackBelowUtil
	d.snap.GPUs = nil
	now := time.Now()
	first := d.addWaiting("first GPU job", 1, 30, 2*time.Minute)
	second := d.addWaiting("second GPU job", 1, 30, time.Minute)
	d.admit(now)
	if first.State != "granted" || second.State != "granted" {
		t.Fatalf("no-card packing uses the utilization and occupied slots at pass start: %s, %s", first.State, second.State)
	}

	cpuFirst := d.addWaiting("first new CPU command", 1, 30, 2*time.Minute)
	cpuSecond := d.addWaiting("second new CPU command", 1, 30, time.Minute)
	for _, j := range []*job{cpuFirst, cpuSecond} {
		j.GPU = false
		delete(d.st.History, histKey(j.Ticket))
	}
	d.admit(now)
	if cpuFirst.State != "granted" || cpuSecond.State != "waiting" || d.waitingDecision(cpuSecond, now).Kind != blockPacing {
		t.Fatalf("CPU pacing must see the first grant in the same pass: %s, %s", cpuFirst.State, cpuSecond.State)
	}
	d.admit(now.Add(5 * time.Second))
	if cpuSecond.State != "granted" {
		t.Fatalf("pacing did not expire after 5 s: %s", cpuSecond.State)
	}
}

func TestGatePassReservesRAMAndGPUPlacement(t *testing.T) {
	d := testDaemon(t)
	d.snap.RAMAvailGB = 8
	d.cfg.GPU.MaxJobs = 1
	d.snap.GPUs = append(d.snap.GPUs, sysinfo.GPU{Vendor: "amd", VRAMGB: 16})
	first := d.addWaiting("NVIDIA job", 2, 30, 3*time.Minute)
	second := d.addWaiting("AMD job", 1, 30, 2*time.Minute)
	second.WantVendor = "amd"
	third := d.addWaiting("CPU job", 2, 30, time.Minute)
	third.GPU = false
	now := time.Now()
	d.admit(now)
	if first.State != "granted" || first.device != 0 || second.State != "granted" || second.device != 1 {
		t.Fatalf("GPU grants: first %s on %d, second %s on %d", first.State, first.device, second.State, second.device)
	}
	if third.State != "waiting" || d.waitingDecision(third, now).Kind != blockRAM || d.startsIn(third, 7, now) != 7 {
		t.Fatalf("the RAM of earlier grants must hold the CPU candidate: %s, %s", third.State, d.blockerOf(third))
	}
	status := d.status()
	if len(status.Waiting) != 1 || status.Waiting[0].ID != third.ID || status.Waiting[0].Why != d.blockerOf(third) {
		t.Fatalf("status does not show the same RAM gate: %+v", status.Waiting)
	}
}
