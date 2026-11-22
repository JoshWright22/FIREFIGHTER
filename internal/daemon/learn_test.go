package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
)

// finishRun runs a job of command cmd to its end with the given measurements, so its history learns.
func (d *Daemon) finishRun(cmd string, gpu bool, peakRAM, peakVRAM, cpus, gpuPct, writtenGB float64, oom bool) {
	started := time.Now().Add(-10 * time.Minute)
	t := &policy.Ticket{ID: NewID(), Project: "p", Name: cmd, CmdHash: cmd, GPU: gpu, Minutes: 10, Bid: 5, Evict: "graceful",
		Band: policy.BandBatch, State: "running", Created: started, Started: started}
	j := newJob(t)
	j.pids[999999] = 1
	j.peakRAM, j.peakVRAM, j.oom = peakRAM, peakVRAM, oom
	j.workSec, j.cpuSec, j.gpuSec = 600, cpus*600, gpuPct*600
	j.written = writtenGB
	d.jobs[t.ID] = j
	d.finish(t.ID, 0, "ok", "")
	delete(d.jobs, t.ID)
}

// The incident's job: declared 1 GB of RAM, used 7.6 GB. After one run the next is admitted on what
// it used, and after a few runs on the 98th percentile of its peaks plus 15%; the declaration no
// longer decides. CPUs, GPU load and disk writes are learned alongside.
func TestCommandNeedsAreLearnedPerResource(t *testing.T) {
	d := testDaemon(t)
	need := func(declRAM float64) policy.Ticket {
		tk := policy.Ticket{Project: "p", CmdHash: "train", GPU: true, RAMGB: declRAM, Minutes: 10}
		d.fillNeeds(&tk)
		return tk
	}
	if got := need(1).NeedRAM; got != 1 {
		t.Fatalf("no history: need = %.1f, want the declared 1", got)
	}
	d.finishRun("train", true, 7.6, 5.0, 3.0, 80, 2.0, false)
	if got := need(1).NeedRAM; got < 9.1 || got > 9.7 {
		t.Fatalf("after one run: need = %.2f, want ~9.1 (7.6 x 1.2)", got)
	}
	d.finishRun("train", true, 7.2, 5.2, 3.2, 84, 2.2, false)
	d.finishRun("train", true, 7.4, 5.1, 2.8, 76, 2.1, false)
	tk := need(1)
	if tk.NeedRAM < 7.6*1.15 || tk.NeedRAM > 7.6*1.15*1.06 {
		t.Fatalf("warm: RAM need = %.2f, want ~%.2f (P98 + 15%%)", tk.NeedRAM, 7.6*1.15)
	}
	if tk.NeedVRAM < 5.2*1.15 || tk.NeedVRAM > 5.2*1.15*1.06 {
		t.Fatalf("warm: VRAM need = %.2f, want ~%.2f", tk.NeedVRAM, 5.2*1.15)
	}
	if tk.NeedCPU < 2.8 || tk.NeedCPU > 3.3 || tk.PredGPU < 76 || tk.PredGPU > 88 || tk.NeedDisk < 2.2 || tk.NeedDisk > 2.8 {
		t.Fatalf("CPUs %.2f, GPU %.0f%%, disk %.2f GB: want ~3, ~80, ~2.5", tk.NeedCPU, tk.PredGPU, tk.NeedDisk)
	}
	// An over-declaration is cut to 1.5 x the highest peak ever seen.
	if got := need(30).NeedRAM; got < 7.6*1.5 || got > 7.6*1.5*1.06 {
		t.Fatalf("declared 30 GB: need = %.2f, want ~%.1f", got, 7.6*1.5)
	}
	// A run stopped for lack of RAM teaches more than it reached (x1.2).
	d.finishRun("train", true, 9.0, 5.0, 3, 80, 2, true)
	if got := need(1).NeedRAM; got < 9.0*1.2*1.15 {
		t.Fatalf("after an out-of-memory stop: need = %.2f, want at least %.2f", got, 9.0*1.2*1.15)
	}
	// It shows in the forecast.
	f := d.forecast(time.Now())
	if len(f.Commands) != 1 || f.Commands[0].Name != "train" || !f.Commands[0].Warm || f.Commands[0].Runs != 4 {
		t.Fatalf("forecast: %+v", f.Commands)
	}
}

// A command never run before starts from what the project's other commands of its kind need.
func TestNewCommandStartsFromItsProjectsHistory(t *testing.T) {
	d := testDaemon(t)
	d.finishRun("train-a", true, 6, 4, 2, 70, 1, false)
	d.finishRun("train-b", true, 8, 6, 2, 70, 1, false)
	d.finishRun("lint", false, 0.3, 0, 1, 0, 0, false)
	tk := policy.Ticket{Project: "p", CmdHash: "train-c", GPU: true}
	d.fillNeeds(&tk)
	// Two siblings: the lower one (6 GB RAM, 4 GB VRAM, x1.2), not the heaviest.
	if tk.NeedRAM < 6*1.2 || tk.NeedRAM > 6*1.2*1.06 || tk.NeedVRAM < 4*1.2 || tk.NeedVRAM > 4*1.2*1.06 {
		t.Fatalf("new GPU command: need %.1f GB RAM / %.1f GB VRAM, want ~7.2 / ~4.8 (the median of the project's GPU commands)", tk.NeedRAM, tk.NeedVRAM)
	}
	other := policy.Ticket{Project: "elsewhere", CmdHash: "x", GPU: true}
	d.fillNeeds(&other)
	if other.NeedRAM != 3 || other.NeedVRAM != 4 {
		t.Fatalf("a project with no history gets the defaults: %.1f / %.1f", other.NeedRAM, other.NeedVRAM)
	}
}

// After a day of history in a state, that state's reserve follows what the human was seen to use,
// within half to three times the configured value; before that the configuration stands.
func TestReserveIsLearnedFromTheHumansUse(t *testing.T) {
	d := testDaemon(t)
	d.user = "active"
	start := time.Now().Add(-48 * time.Hour)
	set := d.cfg.Reserve.Active // 6 CPUs, 10% RAM, 2 GB VRAM
	feed := func(from time.Time, hours float64, cpu func(i int) float64, ramRise, vramRise float64) time.Time {
		h := d.humanUse("active")
		n := int(hours * 3600 / humanEvery.Seconds())
		at := from
		for i := 0; i < n; i++ {
			at = from.Add(time.Duration(i) * humanEvery)
			for _, x := range []interface {
				Age(time.Time, time.Duration)
			}{h.CPU, h.RAMRise, h.VRAMRise} {
				x.Age(at, humanHalfLife)
			}
			h.CPU.Add(cpu(i), 1, at)
			h.RAMRise.Add(ramRise, 1, at)
			h.VRAMRise.Add(vramRise, 1, at)
		}
		return at
	}
	// Two hours of history: not trusted yet.
	end := feed(start, 2, func(int) float64 { return 9 }, 6, 3)
	if got := d.learnedReserve("active", end); got != set {
		t.Fatalf("with 2 h of history the reserve should be the configured one: %+v", got)
	}
	// Two days of a human who compiles in bursts: 1 CPU most of the time, 9 CPUs a tenth of it.
	// By time P95 is 9 only just; by load (the work done) it clearly is: reserve 9 + 2 idle cores.
	end = feed(end, 46, func(i int) float64 {
		if i%10 == 0 {
			return 9
		}
		return 1
	}, 1.2, 0.4)
	got := d.learnedReserve("active", end)
	if got.CPUs < 11 || got.CPUs > 12 {
		t.Fatalf("learned CPUs = %d, want 11 (P95 of the load 9, +2 idle)", got.CPUs)
	}
	// RAM rises at most ~1.2 GB in 5 minutes (P95 over two days, the first 2 h at 6 GB are 4%):
	// 1.2 x 1.15 / 32 GB = 4.3%, but never under half the configured 10%.
	if got.RAMPct != 5 {
		t.Fatalf("learned RAM reserve = %.1f%%, want 5 (half the configured 10%%: the floor)", got.RAMPct)
	}
	if got.VRAMGB != 1 {
		t.Fatalf("learned VRAM reserve = %.1f GB, want 1.0 (half the configured 2 GB: the floor)", got.VRAMGB)
	}
	// The control loop uses it, and status says where it came from.
	d.res = policy.ReserveCtl{}
	d.res.Update(d.withLearnedReserve(d.cfg, end), d.user, 0, d.snap.CPUs, end)
	if d.res.CPUs != float64(got.CPUs) || d.res.RAMPct != 5 {
		t.Fatalf("reserve in force: %+v", d.res)
	}
	f := d.forecast(end)
	var active HumanForecast
	for _, h := range f.Human {
		if h.State == "active" {
			active = h
		}
	}
	if !active.Learned || !active.Now || active.Days < 1.9 || active.SetCPUs != 6 || active.CPUs != got.CPUs {
		t.Fatalf("forecast for active: %+v", active)
	}
	// Off: the configuration again.
	d.cfg.Reserve.Learn = false
	if d.learnedReserve("active", end) != set {
		t.Fatal("reserve.learn = false must give the configured reserve")
	}
}

// A job whose past runs keep many CPUs busy waits when the CPUs outside the human's reserve are
// taken by jobs already running; a job with no history is never held for CPUs.
func TestAdmissionCountsLearnedCPUs(t *testing.T) {
	d := testDaemon(t) // 16 CPUs, present: 4 reserved -> 12 for jobs, 15 with the 125% allowance
	now := time.Now()
	run := d.addRunning("build", 1, now.Add(-time.Minute))
	run.GPU, run.NeedCPU, run.cpuNow = false, 10, 10
	big := d.addWaiting("render", 1, 30, time.Minute)
	big.GPU, big.NeedCPU = false, 8
	fresh := d.addWaiting("unknown", 1, 30, time.Minute)
	fresh.GPU = false
	d.admit(now)
	if big.State != "waiting" || !strings.Contains(d.blocker(big), "usually keeps 8.0 CPUs busy") {
		t.Fatalf("render: state %s, blocker %q", big.State, d.blocker(big))
	}
	if fresh.State != "granted" {
		t.Fatalf("a job with no CPU history must not wait for CPUs: %s", d.blocker(fresh))
	}
	run.State = "done"
	d.admit(now)
	if big.State != "granted" {
		t.Fatalf("render should start once the build is done: %s", d.blocker(big))
	}
}

// Two GPU jobs share a card only when their loads fit together (Gandiva): a job that usually loads
// the GPU to 80% takes a turn instead of joining one already at 60%.
func TestGPUPackingUsesLearnedLoad(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.addRunning("train", 1, now.Add(-time.Minute))
	d.cardUtil[0] = 60
	heavy := d.addWaiting("eval-heavy", 1, 30, time.Minute)
	heavy.PredGPU = 80
	light := d.addWaiting("eval-light", 1, 30, 2*time.Minute)
	light.PredGPU = 20
	d.admit(now)
	if heavy.State != "waiting" || light.State != "granted" {
		t.Fatalf("heavy %s, light %s: want the light one packed and the heavy one waiting", heavy.State, light.State)
	}
}

// Borg's reservation: a running job's unused request is held back in full for its first 300 s of
// work, then decays towards what it really uses (+15%), halving every 10 minutes.
func TestUnusedRequestDecaysTowardsRealUse(t *testing.T) {
	d := testDaemon(t)
	j := d.addRunning("asked for 12", 12, time.Now().Add(-30*time.Minute)) // within its 60-minute lease
	j.ramGB, j.peakRAM = 6, 6
	j.workSec = 200
	if got := d.reservedRAM(j); got != 6 {
		t.Fatalf("during start-up the whole unused request is reserved: %.2f, want 6", got)
	}
	j.workSec = 300 + 600 // one half-life: 6.9 + (12-6.9)/2 = 9.45 -> 3.45 beyond its 6
	if got := d.reservedRAM(j); got < 3.4 || got > 3.5 {
		t.Fatalf("after 15 min: %.2f, want 3.45", got)
	}
	j.workSec = 300 + 6000
	if got := d.reservedRAM(j); got < 0.89 || got > 0.92 {
		t.Fatalf("settled: %.2f, want 0.9 (15%% of its 6 GB peak)", got)
	}
	j.ramGB, j.peakRAM = 13, 13 // it grew past its request: nothing unused, and no negative reserve
	if got := d.reservedRAM(j); got != 0 {
		t.Fatalf("over its request: %.2f, want 0", got)
	}
}

// 2026-11-14: a 9 GB training run waited four hours on an idle machine because its need had been
// set to 26.7 GB, more than the PC ever has free, and status only said "spot band". A need is never
// counted above what the machine can give, the status line says what happened, and being out of
// budget never keeps a job off spare capacity.
func TestNeedIsCappedAtWhatTheMachineCanGiveAndSpotStillRuns(t *testing.T) {
	d := testDaemon(t)
	d.user = "away"
	d.res = policy.ReserveCtl{}
	d.res.Update(d.cfg, "away", 0, 16, time.Now())
	d.ramBase = 9 // the OS and the human's open programs never go under 9 GB
	// One heavy run of this very command: 22 GB peak -> 26.4 GB wanted.
	j := d.addWaiting("ctl-port", 1, 90, time.Minute)
	d.finishRun("ctl-port", true, 22, 2, 2, 60, 0, false)
	d.fillNeeds(j.Ticket)
	most := 32*0.95 - 9 - 1.6 // 19.8 GB: total less the away reserve, the floor in use, the margin
	if j.NeedRAM < most-0.01 || j.NeedRAM > most+0.01 || j.WantRAM < 26 {
		t.Fatalf("need %.1f GB (wanted %.1f): want it cut to %.1f, the most the machine frees", j.NeedRAM, j.WantRAM, most)
	}
	j.Band = policy.BandSpot // its project is out of budget
	d.snap.RAMAvailGB = 18
	d.admit(time.Now())
	if j.State != "waiting" {
		t.Fatalf("18 GB free can't hold a %.1f GB need plus the margin", j.NeedRAM)
	}
	why := d.blocker(j)
	for _, want := range []string{"needs 19.8 GB RAM", "its past runs point to", "more than this machine ever has free", "starts when the machine is at its emptiest", "it still runs whenever there is room"} {
		if !strings.Contains(why, want) {
			t.Fatalf("status line lacks %q:\n%s", want, why)
		}
	}
	if strings.HasPrefix(why, "spot") {
		t.Fatalf("the budget is not the reason it waits:\n%s", why)
	}
	// The machine at its emptiest (about 23 GB free = 32 - 9): it starts, out of budget or not.
	d.snap.RAMAvailGB = 23.5
	d.admit(time.Now())
	if j.State != "granted" {
		t.Fatalf("away, idle GPU, room for it: an out-of-budget job must start: %s", d.blocker(j))
	}
	// A command with no history is counted at what it declares.
	k := policy.Ticket{Project: "p", CmdHash: "other", GPU: true, RAMGB: 10}
	d.fillNeeds(&k)
	if k.NeedRAM != 10 || k.WantRAM != 0 {
		t.Fatalf("declared 10 GB: need %.1f (want %.1f)", k.NeedRAM, k.WantRAM)
	}
}

// A project in debt on its first day gets its refill even when the pool for new projects is empty:
// paying off a debt mints nothing. (nanoTSFM sat at -358.8 for hours on 2026-11-14.) Only what
// would lift it above zero comes out of the pool.
func TestDebtRefillsOnTheFirstDay(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	created := now.Add(-10 * time.Hour)
	d.st.Accounts["nanoTSFM"] = &policy.Account{Bucket: -100, BucketAt: now.Add(-2 * time.Hour), DecayAt: now, Created: created}
	d.st.Accounts["other"] = &policy.Account{Bucket: 50, BucketAt: now, DecayAt: now, Created: created}
	d.st.Newcomers, d.st.NewcomersAt = 0, now // the shared pool is spent
	a := d.account("nanoTSFM", now)
	if a.Bucket < -80.1 || a.Bucket > -79.9 {
		t.Fatalf("after 2 h at 10/h the debt should be -80, got %.1f", a.Bucket)
	}
	// Almost paid off, 2 more hours, pool still empty: it reaches zero and stops there.
	a.Bucket, a.BucketAt = -5, now.Add(-2*time.Hour)
	if a = d.account("nanoTSFM", now); a.Bucket != 0 {
		t.Fatalf("with the pool empty a first-day project refills to 0, not past it: %.1f", a.Bucket)
	}
	d.st.Newcomers = 8
	a.Bucket, a.BucketAt = -5, now.Add(-2*time.Hour)
	if a = d.account("nanoTSFM", now); a.Bucket < 7.9 || a.Bucket > 8.1 || d.st.Newcomers > 0.1 {
		t.Fatalf("above zero it draws on the pool (8 left): bucket %.1f, pool %.1f", a.Bucket, d.st.Newcomers)
	}
}

// A job the guard had to stop for the human's RAM is not billed for the work it lost.
func TestStoppedForRAMIsRefunded(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("victim", 9, now.Add(-6*time.Minute))
	a := d.account("p", now)
	before := a.Bucket
	a.Charge(d.cfg, 17.8, 0, 0, 0, 1)
	j.charged, j.oom, j.killedAt, j.reason = 17.8, true, now, "RAM critically low"
	d.finish(j.ID, 137, "evicted", "")
	if got := d.account("p", now).Bucket; got < before-0.01 || j.receipt.Charged != 0 {
		t.Fatalf("budget %.1f (was %.1f), receipt charged %.1f: the charge should be refunded", got, before, j.receipt.Charged)
	}
}
