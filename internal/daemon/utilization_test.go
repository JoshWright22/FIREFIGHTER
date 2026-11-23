package daemon

// The 2026-11-22 redesign: a reserve for basic use, commit that may grow, declarations believed,
// bots behind project work, real places in line (Josh 11/22).

import (
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// Outside games the reserve covers basic use (at most reserve.basic_gb), not min_free_gb, and no
// extra margin is kept on top: with 8.5 GB free, a 5 GB GPU job starts.
func TestBasicReserveLeavesRoomForJobs(t *testing.T) {
	d := testDaemon(t)
	d.cfg.Reserve.Auto = true
	d.cfg.Reserve.MinFreeGB = 4 // the setting that held everything back
	d.user = "active"
	d.res.Update(d.withLearnedReserve(d.cfg, time.Now()), d.user, 0, 16, time.Now())
	d.snap.RAMAvailGB = 8.5
	if f := d.ramFloor(); f > 3.01 || f < 1.5 {
		t.Fatalf("basic-use reserve %.2f GB, want 1.5-3", f)
	}
	if m := d.admitMargin(); m != 0 {
		t.Fatalf("no extra margin under reserve.auto, got %.1f", m)
	}
	j := d.addWaiting("transfer job", 5, 30, time.Minute)
	d.admit(time.Now())
	if j.State != "granted" {
		t.Fatalf("a 5 GB job should start with 8.5 GB free: %s (%s)", j.State, d.blocker(j))
	}
	// In a game the configured reserve is back: min_free_gb holds.
	d.user = "game"
	if f := d.ramFloor(); f < 4 {
		t.Fatalf("game reserve %.1f, want min_free_gb 4", f)
	}
}

// Light use (browsing at 3.9 CPUs) keeps the CPU reserve at its level; real load beyond
// reserve.basic_cpus makes it follow.
func TestCPUReserveFollowsOnlyHeavyUse(t *testing.T) {
	d := testDaemon(t)
	d.cfg.Reserve.Auto = true
	d.user = "active"
	now := time.Now()
	d.res = policy.ReserveCtl{}
	d.res.Update(d.withLearnedReserve(d.cfg, now), "active", 3.9, 16, now)
	if d.res.CPUs > float64(d.cfg.Reserve.BasicCPUs) {
		t.Fatalf("browsing kept %.0f CPUs, want at most %d", d.res.CPUs, d.cfg.Reserve.BasicCPUs)
	}
	d.res.Update(d.withLearnedReserve(d.cfg, now), "active", 9, 16, now)
	if d.res.CPUs < 11 {
		t.Fatalf("a 9-CPU build must raise the reserve to 11, got %.0f", d.res.CPUs)
	}
}

// Commit near its current limit is no shortage while Windows may still grow the page file; a
// page file that can't grow still bounds new jobs.
func TestCommitBindsOnlyWhenThePageFileCantGrow(t *testing.T) {
	d := testDaemon(t)
	d.cfg.Reserve.CommitFreeGB = 3
	d.snap.RAMAvailGB = 12
	d.snap.CommitLimitGB, d.snap.CommitUsedGB, d.snap.CommitMaxGB = 48, 42, 126
	if r := d.ramParts(); r.commitBound() {
		t.Fatalf("growable page file: commit must not bind (%s)", r)
	}
	d.snap.CommitMaxGB = 48 // fixed size
	r := d.ramParts()
	if !r.commitBound() || r.left() > 3.01 {
		t.Fatalf("fixed page file: commit binds at 6-3 = 3 GB, got %.1f (%s)", r.left(), r)
	}
	if !strings.Contains(r.String(), "Windows commit") {
		t.Fatalf("the breakdown must name commit: %s", r)
	}
}

// A declaration below a learned estimate is what admission counts (a mixed history said 17.3 GB
// for jobs peaking at 6-7); without one, the history still rules.
func TestDeclaredRAMBeatsAnInflatedHistory(t *testing.T) {
	d := testDaemon(t)
	h := &History{RAM: usage.NewHist(0.05)}
	for i := 0; i < 3; i++ {
		usage.AddRun(h.RAM, 15, 10, time.Now()) // a bigger config's runs under the same command
	}
	usage.AddRun(h.RAM, 5.8, 10, time.Now()) // and one of this size, which fit in 6.5
	tk := &policy.Ticket{Project: "nanoTSFM", CmdHash: "fresh", GPU: true, RAMGB: 6.5}
	d.st.History[histKey(tk)] = h
	d.fillNeeds(tk)
	if tk.NeedRAM != 6.5 || tk.LearnedRAM < 15 {
		t.Fatalf("need %.1f learned %.1f, want 6.5 and >= 15", tk.NeedRAM, tk.LearnedRAM)
	}
	j := newJob(tk)
	j.State = "waiting"
	if why := blockerText(j, "needs 6.5 GB RAM; 0.0 GB free"); !strings.Contains(why, "declared --ram 6.5") || strings.Contains(why, "declare --ram to set") {
		t.Fatalf("message: %s", why)
	}
	none := &policy.Ticket{Project: "nanoTSFM", CmdHash: "fresh", GPU: true}
	d.fillNeeds(none)
	if none.NeedRAM < 15 {
		t.Fatalf("undeclared: the history rules, got %.1f", none.NeedRAM)
	}
}

// A job past its lease, or a bot's started job, holds back only the RAM it really uses.
func TestReservationEndsPastTheLease(t *testing.T) {
	d := testDaemon(t)
	j := d.addRunning("hub-apply batch", 0.3, time.Now().Add(-40*time.Minute))
	j.NeedRAM, j.Minutes, j.workSec = 1.4, 30, 100
	if r := d.reservedRAM(j); r != 0 {
		t.Fatalf("past its lease: reserved %.1f, want 0", r)
	}
	j.Minutes = 120
	if r := d.reservedRAM(j); r < 1 {
		t.Fatalf("within its lease the reservation holds: %.1f", r)
	}
	j.Background, j.workSec = true, 200
	if r := d.reservedRAM(j); r != 0 {
		t.Fatalf("a bot's started job: reserved %.1f, want 0", r)
	}
}

// Unattended recurring CPU-only work is learned as background; a project with GPU work, or one
// an agent drives, is not; the config lists win.
func TestBackgroundIsLearned(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	for i := 0; i < 8; i++ {
		d.noteSubmit("hub-apply", false, false, now.Add(-time.Duration(i)*time.Hour))
		d.noteSubmit("nanoTSFM", false, true, now.Add(-time.Duration(i)*time.Hour))
		d.noteSubmit("firefighter", true, false, now.Add(-time.Duration(i)*time.Hour))
	}
	if bg, _ := d.background("hub-apply", now); !bg {
		t.Fatal("an unattended recurring CPU loop is background")
	}
	for _, p := range []string{"nanoTSFM", "firefighter", "new"} {
		if bg, _ := d.background(p, now); bg {
			t.Fatalf("%s must not be background", p)
		}
	}
	d.cfg.Order.Foreground = []string{"hub-apply"}
	d.cfg.Order.Background = []string{"firefighter"}
	if bg, _ := d.background("hub-apply", now); bg {
		t.Fatal("order.foreground wins")
	}
	if bg, _ := d.background("firefighter", now); !bg {
		t.Fatal("order.background wins")
	}
}

// Places in line are the real admission order: no two jobs share a number.
func TestPositionsAreDistinct(t *testing.T) {
	d := testDaemon(t)
	d.snap.RAMAvailGB = 2 // nothing starts
	a := d.addWaiting("a", 9, 90, 3*time.Minute)
	b := d.addWaiting("b", 9, 90, 2*time.Minute)
	c := d.addWaiting("c", 9, 90, time.Minute)
	a.Bid, b.Bid, c.Bid = 7, 5, 7
	now := time.Now()
	got := []int{d.position(a, now), d.position(b, now), d.position(c, now)}
	if got[0] == got[2] || got[1] != 3 {
		t.Fatalf("positions %v: want distinct, bid 5 last", got)
	}
}

// A declaration below every run ever measured is not believed (the 11/13 job: declared 1, used 7.6).
func TestDeclarationBelowEveryRunIsOverruled(t *testing.T) {
	d := testDaemon(t)
	h := &History{RAM: usage.NewHist(0.05), RAMMeasure: ramMeasureShared}
	usage.AddRun(h.RAM, 7.6, 10, time.Now())
	tk := &policy.Ticket{Project: "p", CmdHash: "train", GPU: true, RAMGB: 1}
	d.st.History[histKey(tk)] = h
	d.fillNeeds(tk)
	if tk.NeedRAM < 9 {
		t.Fatalf("need %.1f: a 1 GB declaration for a 7.6 GB command must not count", tk.NeedRAM)
	}
}

// Josh 11/22: "reserve for the human: 13 CPUs" while he browsed: agents' processes and a job's
// not-yet-attributed children counted as his. The reserve follows his own measured use only,
// stays at basic use until that passes reserve.basic_cpus, and comes back down at once.
func TestCPUReserveFollowsOnlyTheHumansOwnUse(t *testing.T) {
	d := testDaemon(t)
	d.cfg.Reserve.Auto = true
	d.cfg.Reserve.MaxCPUPct = 90
	d.user = "active"
	now := time.Now()
	d.res = policy.ReserveCtl{}
	step := func(at time.Time) { d.res.Update(d.withLearnedReserve(d.cfg, at), d.user, d.humanCPULoad(), 16, at) }
	d.busyUser = 11                            // everything that isn't a measured job: agents, unattributed job children
	d.humanCPU.ok, d.humanCPU.ewma = true, 1.5 // the human's own processes
	step(now)
	if d.res.CPUs > float64(d.cfg.Reserve.BasicCPUs) {
		t.Fatalf("agents' load lifted the reserve to %.0f CPUs", d.res.CPUs)
	}
	d.humanCPU.ewma = 7 // the human starts a build
	step(now.Add(time.Second))
	if d.res.CPUs < 9 {
		t.Fatalf("the human's own 7-CPU build must raise the reserve: %.0f", d.res.CPUs)
	}
	d.humanCPU.ewma = 1
	step(now.Add(20 * time.Second))
	if d.res.CPUs > float64(d.cfg.Reserve.BasicCPUs) {
		t.Fatalf("back to light use, the reserve should drop at once: %.0f", d.res.CPUs)
	}
	// A fresh guard (no scan yet) treats the load as light.
	d.humanCPU.ok = false
	d.res = policy.ReserveCtl{}
	step(now.Add(40 * time.Second))
	if d.res.CPUs > float64(d.cfg.Reserve.BasicCPUs) {
		t.Fatalf("a restart lifted the reserve to %.0f", d.res.CPUs)
	}
}

// UnomiBackend 11/22: a job's script ran `docker run --rm --name t4sim-...`; when the job was
// stopped for RAM, its python and docker CLI died but the container ran on in the VM, counted as
// the human's load, and the requeued job was stopped again and again. A container the job's own
// docker run named is the job's: it is killed with the job. One only guessed by timing isn't, but
// it is shown as the job's leftover, never as the human's load.
func TestStoppedJobsContainerGoesWithIt(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("T4 fixes: image defaults only", 1, now.Add(-time.Minute))
	cs := []sysinfo.Container{{ID: "11c91cf30bda", Name: "t4sim-1795415112", MemGB: 6.3, Created: now.Add(-50 * time.Second)}}
	lines := map[string][]string{j.ID: {"docker run --rm --name t4sim-1795415112 --gpus all img python t4sim.py"}}
	d.adoptContainers(cs, lines, map[string]time.Time{j.ID: now.Add(-55 * time.Second)})
	if j.container != "11c91cf30bda" {
		t.Fatalf("the job's named container wasn't taken as its own: %q", j.container)
	}
	j.reason = "RAM critically low"
	d.killJob(j, now)
	d.finish(j.ID, -1, "killed", "")
	killed := false
	for len(d.dockerQ) > 0 {
		if args := strings.Join(<-d.dockerQ, " "); args == "docker kill 11c91cf30bda" {
			killed = true
		}
	}
	if !killed {
		t.Fatal("the stopped job's container wasn't killed")
	}

	// Guessed by timing only: not killed, but not the human's either.
	g := d.addRunning("guessed", 1, now.Add(-time.Minute))
	g.ctrSeen = "aaaaaaaaaaaa"
	d.others = []sysinfo.Container{{ID: "aaaaaaaaaaaa", Name: "busy_turing", MemGB: 4}}
	d.finish(g.ID, -1, "killed", "")
	if out := d.outsideContainers(); len(out) != 0 {
		t.Fatalf("a finished job's leftover counted as the human's load: %v", out)
	}
	if n := strings.Join(d.notes(now), "\n"); !strings.Contains(n, "busy_turing is still running after its job (guessed)") {
		t.Fatalf("notes: %s", n)
	}
	d.others = nil // gone
	if left := d.leftoverContainers(); len(left) != 0 || len(d.leftover) != 0 {
		t.Fatalf("a gone container is forgotten: %v", left)
	}
}

// The do: line names the amend that would start a job whose declaration is just over what fits.
func TestDoLineNamesTheAmendThatStartsIt(t *testing.T) {
	d := testDaemon(t)
	d.snap.RAMAvailGB = 2.56 + 11.0 // 11.0 GB for jobs
	d.cfg.Reserve.RAMMarginPct = 0
	j := d.addWaiting("t4 fixes", 12, 30, time.Minute)
	j.RAMGB = 12
	v := d.view(j)
	if v.Quote == nil || !strings.Contains(v.Quote.Do, "firefighter amend "+j.ID+" --ram 11") {
		t.Fatalf("do: %+v", v.Quote)
	}
}

// UnomiBackend 11/22-23: reclaims that ran while the job's container kept reading files came back
// empty, and their backoff kept the VM from being asked again once the job was suspended. A
// suspension for RAM asks the VM again at once.
func TestSuspendingForRAMAsksTheVMAgain(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("t4sim", 6, now.Add(-time.Minute))
	d.vm = sysinfo.VM{Name: "docker-desktop", HostGB: 13.4, UsedGB: 6.8, CacheGB: 6.6, Guest: true}
	d.reclaim.misses, d.reclaim.end = 3, now.Add(-10*time.Second)
	d.hold(j, "ram", "RAM: 1.7 GB free", now)
	if d.reclaim.misses != 0 || !d.reclaim.end.IsZero() {
		t.Fatalf("the reclaim backoff wasn't reset: misses %d end %v", d.reclaim.misses, d.reclaim.end)
	}
}

// nanoTSFM 11/23 (bug f30ba831): --ram 12 --vram 7 counted as 13.9 GB RAM and the whole 16.4 GB card,
// from one earlier run measured at 20 GB RAM (its DataLoader workers' shared memory-mapped dataset
// counted once per worker) and 13.5 GB VRAM (PyTorch's cache).
func TestDeclarationsBeatInflatedMeasurements(t *testing.T) {
	d := testDaemon(t)
	h := &History{RAM: usage.NewHist(0.05), VRAM: usage.NewHist(0.05)} // measured the old way
	usage.AddRun(h.RAM, 20, 10, time.Now())
	usage.AddRun(h.VRAM, 13.5, 10, time.Now())
	tk := &policy.Ticket{Project: "nanoTSFM", CmdHash: "clean_single", GPU: true, RAMGB: 12, VRAMGB: 7}
	d.st.History[histKey(tk)] = h
	d.fillNeeds(tk)
	if tk.NeedRAM != 12 || tk.NeedVRAM != 7 {
		t.Fatalf("counted RAM %.1f VRAM %.1f, want the declared 12 and 7", tk.NeedRAM, tk.NeedVRAM)
	}
	// A run that ran out of VRAM: a lower --vram is no longer believed.
	h.VRAMOOM = true
	d.fillNeeds(tk)
	if tk.NeedVRAM < 13.5 {
		t.Fatalf("after an out-of-VRAM run, a lower declaration must not count: %.1f", tk.NeedVRAM)
	}
	// The next finished run replaces the old-way peaks and is marked measured the new way.
	j := newJob(tk)
	j.peakRAM, j.GPU = 4.2, true
	d.learnRun(h, j, time.Now())
	if h.RAMMeasure != ramMeasureShared || h.RAM.N != 1 || h.RAM.Max() > 5 {
		t.Fatalf("history after a new-way run: measure %d, n %.0f, max %.1f", h.RAMMeasure, h.RAM.N, h.RAM.Max())
	}
}

// Review gate 11/23 on f30ba831: (1) a believed low --vram holds only with the card to itself;
// beside another GPU job, and in what it reserves against others, the learned figure counts;
// (2) VRAMOOM only from a real out-of-VRAM end, cleared by a clean run; (3) old-way RAM peaks are
// discounted, not ignored: --ram 1 for a command measured at 7.6 GB is still overruled.
func TestLowDeclarationsAreBoundedByWhatTheyCanHurt(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	h := &History{VRAM: usage.NewHist(0.05), RAM: usage.NewHist(0.05)}
	usage.AddRun(h.VRAM, 12, 10, now)
	usage.AddRun(h.RAM, 7.6, 10, now)
	tk := &policy.Ticket{ID: NewID(), Project: "p", CmdHash: "train", Name: "train", GPU: true, VRAMGB: 7, RAMGB: 1,
		Minutes: 30, Bid: 5, Band: policy.BandBatch, State: "waiting", Created: now}
	d.st.History[histKey(tk)] = h
	d.fillNeeds(tk)
	if tk.NeedVRAM != 7 || tk.LearnedVRAM < 12 {
		t.Fatalf("VRAM need %.1f learned %.1f: want 7 and >= 12", tk.NeedVRAM, tk.LearnedVRAM)
	}
	if tk.NeedRAM < 7.6 {
		t.Fatalf("--ram 1 for a command measured at 7.6 GB (old way) must not count: %.1f", tk.NeedRAM)
	}
	j := newJob(tk)
	d.jobs[tk.ID] = j
	// Alone on the card (16 GB, 1 used, 1 reserved: 14 free): its 7 GB, and the learned 12, both fit.
	// Beside a running job holding 5 GB (9 free): 7 fits, its learned 12 doesn't, so it waits.
	other := d.addRunning("other", 2, now.Add(-time.Minute))
	other.vramGB, other.NeedVRAM = 5, 5
	d.snap.GPUs[0].UsedGB = 6
	d.cfg.GPU.MaxJobs = 2
	if dec := d.waitingDecision(j, now); dec.Kind != blockVRAM {
		t.Fatalf("beside another GPU job its learned VRAM must count: %v %s", dec.Kind, dec.Text)
	}
	delete(d.jobs, other.ID)
	d.snap.GPUs[0].UsedGB = 1
	if dec := d.waitingDecision(j, now); dec.Kind == blockVRAM {
		t.Fatalf("alone on the card its --vram 7 counts: %s", dec.Text)
	}
	// What a running believed-low job reserves against others is its learned figure.
	j.State, j.Started, j.vramGB = "running", now, 3
	if r := d.reservedVRAM(j); r < 8.9 {
		t.Fatalf("reserved %.1f GB, want its learned 12 less the 3 it holds", r)
	}
	// VRAMOOM: an OOM line far under its need doesn't set it; a real one does; a clean run clears it.
	j.NeedVRAM, j.vramOOM, j.peakVRAM = 7, true, 2
	d.learnRun(h, j, now)
	if h.VRAMOOM {
		t.Fatal("an out-of-VRAM line at 2 of 7 GB set the flag")
	}
	j.peakVRAM = 6.9
	d.learnRun(h, j, now)
	if !h.VRAMOOM {
		t.Fatal("a real out-of-VRAM end didn't set the flag")
	}
	j.vramOOM, j.endedOK = false, true
	d.learnRun(h, j, now)
	if h.VRAMOOM {
		t.Fatal("a clean run didn't clear the flag")
	}
}
