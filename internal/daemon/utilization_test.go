package daemon

// The 2026-11-22 redesign: a reserve for basic use, commit that may grow, declarations believed,
// bots behind project work, real places in line (Josh 11/22).

import (
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
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
	h := &History{RAM: usage.NewHist(0.05)}
	usage.AddRun(h.RAM, 7.6, 10, time.Now())
	tk := &policy.Ticket{Project: "p", CmdHash: "train", GPU: true, RAMGB: 1}
	d.st.History[histKey(tk)] = h
	d.fillNeeds(tk)
	if tk.NeedRAM < 9 {
		t.Fatalf("need %.1f: a 1 GB declaration for a 7.6 GB command must not count", tk.NeedRAM)
	}
}
