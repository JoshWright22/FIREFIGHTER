package daemon

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// testDaemon is a guard with no loop and no real machine behind it: a 32 GB, 16-CPU PC with one
// 16 GB card, the human present. Tests set readings and call the ladder and admission directly.
func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FIREFIGHTER_HOME", home)
	t.Setenv("FIREFIGHTER_FAKE_FILE", filepath.Join(home, "fake.json")) // never the real WSL VM
	if err := os.WriteFile(filepath.Join(home, "fake.json"), []byte(`{"ram_avail_gb": 17}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{jobs: map[string]*job{}, st: load(), notify: make(chan struct{}), user: "present",
		stop: make(chan struct{}), dockerQ: make(chan []string, 8)}
	d.cfg = config.Defaults()
	d.cfg.Guard.AutoAdopt = false
	d.cfg.Reserve.MinFreeGB = 0 // the tests below do their arithmetic on the percentage reserve; floor tests set it
	d.cfgShared.Store(&d.cfg)
	d.snap = sysinfo.Snapshot{CPUs: 16, RAMTotalGB: 32, RAMAvailGB: 20, Idle: 60,
		GPUs: []sysinfo.GPU{{Name: "test", Vendor: "nvidia", VRAMGB: 16, UsedGB: 1}}}
	d.cardFloor, d.cardUtil = []float64{1}, []float64{0}
	d.res.Update(d.cfg, d.user, 0, 16, time.Now())
	return d
}

func (d *Daemon) addRunning(name string, ramGB float64, started time.Time) *job {
	t := &policy.Ticket{ID: NewID(), Project: "p", Name: name, GPU: true, Minutes: 60, Bid: 5, Evict: "graceful",
		Band: policy.BandBatch, State: "running", Created: started, Started: started, NeedRAM: ramGB, NeedVRAM: 4, PredMinutes: 60}
	j := newJob(t)
	j.ramGB = ramGB
	d.jobs[t.ID] = j
	return j
}

func (d *Daemon) addWaiting(name string, needRAM float64, minutes float64, age time.Duration) *job {
	t := &policy.Ticket{ID: NewID(), Project: "p", Name: name, GPU: true, Minutes: minutes, Bid: 5, Evict: "graceful",
		Band: policy.BandBatch, State: "waiting", Created: time.Now().Add(-age), NeedRAM: needRAM, NeedVRAM: 2, PredMinutes: minutes,
		CmdHash: name}
	j := newJob(t)
	d.jobs[t.ID] = j
	d.st.History[histKey(t)] = &History{Runs: 1} // a known command: no first-run spacing
	return j
}

func daemonLog(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(config.Home(), "daemon.log"))
	return string(b)
}

// The incident of 2026-11-13: a Docker build's file cache in the WSL VM took Windows to 0.5 GB
// free. That cache is reclaimed; the job is not touched while the reclaim runs, and not after it
// worked.
func TestVMCacheIsReclaimedBeforeAnyJobIsTouched(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("scoring pass", 7.6, now.Add(-6*time.Minute))
	d.snap.RAMAvailGB = 0.5 // critical: under 3% of 32 GB
	d.vm, d.vmAt = sysinfo.VM{Name: "docker-desktop", HostGB: 19, UsedGB: 1, CacheGB: 18, Guest: true}, now
	d.ramLow = now.Add(-time.Minute)

	step := func(at time.Time) { // the reclaim reports back from its own goroutine
		d.mu.Lock()
		defer d.mu.Unlock()
		d.ladder(at)
	}
	reclaiming := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.reclaim.on
	}
	step(now)
	if !reclaiming() {
		t.Fatalf("no reclaim started:\n%s", daemonLog(t))
	}
	for i := 1; i <= 20; i++ { // 10 s of critical RAM while the reclaim runs
		step(now.Add(time.Duration(i) * tick))
	}
	if !j.killedAt.IsZero() || !j.suspended.IsZero() || !j.checkpoint.IsZero() {
		t.Fatalf("job touched while the reclaim was running:\n%s", daemonLog(t))
	}
	if log := daemonLog(t); !strings.Contains(log, "give back its 18.0 GB of file cache before any job is touched") {
		t.Fatalf("the log doesn't say what was reclaimed:\n%s", log)
	}
	d.mu.Lock()
	notes := strings.Join(d.notes(now), "\n")
	d.mu.Unlock()
	if !strings.Contains(notes, "giving back its file cache") {
		t.Fatalf("status doesn't mention the reclaim: %v", notes)
	}
	for end := time.Now().Add(20 * time.Second); reclaiming() && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond) // until the reclaim has reported back
	}
	// The reclaim asked the VM (recorded by the scripted VM), and RAM came back.
	if b, _ := os.ReadFile(os.Getenv("FIREFIGHTER_FAKE_FILE") + ".reclaim"); len(b) == 0 {
		t.Fatal("the VM was never asked to drop its cache")
	}
	d.snap.RAMAvailGB = 17
	step(now.Add(11 * time.Second))
	if !j.killedAt.IsZero() || j.held != "" {
		t.Fatalf("job touched after the reclaim fixed it:\n%s", daemonLog(t))
	}
}

// Real pressure with nothing to reclaim: the job is suspended (its RAM moves to the pagefile on
// Windows), never killed while that works, and resumes when RAM is back. Its receipt says so.
func TestRAMPressureSuspendsInsteadOfKilling(t *testing.T) {
	if !sysinfo.CanTrim {
		t.Skip("suspending frees RAM only where a process's memory can be moved out (Windows)")
	}
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("training", 7.6, now.Add(-6*time.Minute))
	d.snap.RAMAvailGB = 0.5
	d.ramLow = now.Add(-time.Minute)
	d.ladder(now)
	if j.held != "ram" || j.suspended.IsZero() || !j.killedAt.IsZero() {
		t.Fatalf("critical RAM should suspend the job, not kill it (held %q):\n%s", j.held, daemonLog(t))
	}
	j.ramGB = 0.1 // the trim worked
	d.snap.RAMAvailGB = 8
	now = now.Add(4 * time.Second)
	d.ladder(now)
	if j.freedGB < 7 || j.trimFailed {
		t.Fatalf("freed %.1f GB, trimFailed %v", j.freedGB, j.trimFailed)
	}
	if ram, _ := d.headroom(now); ram > 8-32*0.08-7.4 {
		t.Fatalf("headroom %.1f GB doesn't keep room for the suspended job's 7.6 GB", ram)
	}
	// Not enough room to take its 7.5 GB back yet: it stays suspended.
	now = now.Add(30 * time.Second)
	d.ladder(now)
	d.ladder(now.Add(6 * time.Second))
	if j.held != "ram" {
		t.Fatal("resumed without room for its RAM")
	}
	d.snap.RAMAvailGB = 20
	now = now.Add(10 * time.Second)
	d.ladder(now)
	d.ladder(now.Add(6 * time.Second))
	if j.held != "" || !j.suspended.IsZero() || !j.pausedAt.IsZero() {
		t.Fatalf("not resumed with RAM back:\n%s", daemonLog(t))
	}
	if j.pausedTotal < 40*time.Second {
		t.Fatalf("held time counted against its lease: paused %s", j.pausedTotal)
	}
	d.finish(j.ID, 0, "ok", "")
	if r := j.receipt; r.Held != 1 || r.FreedRAM < 7 || r.HeldMinutes <= 0 {
		t.Fatalf("receipt doesn't show the suspension: %+v", r)
	}
	log := daemonLog(t)
	for _, want := range []string{"suspended, not stopped", "freed 7.5 GB of RAM", "resumed after"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
}

// A job is stopped for RAM only when suspending it freed nothing, RAM is still critical, and its
// RAM covers what is missing. The reason and the RAM freed are in the log and the receipt.
func TestKillOnlyWhenItCoversTheShortfall(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	small := d.addRunning("small", 0.3, now.Add(-10*time.Minute))
	d.snap.RAMAvailGB = 0.2 // 0.76 GB short of the 3% line; the job holds 0.3 GB
	d.ramLow = now.Add(-time.Minute)
	for i := 0; i < 80; i++ { // 40 s
		now = now.Add(tick)
		d.ladder(now) // (on Windows it's suspended; its RAM reading stays: the trim "failed")
	}
	if !small.killedAt.IsZero() {
		t.Fatalf("killed a job whose 0.3 GB can't cover a 0.76 GB shortfall:\n%s", daemonLog(t))
	}
	if !strings.Contains(daemonLog(t), "stopping them would not fix it") {
		t.Fatalf("the log doesn't say why nothing was stopped:\n%s", daemonLog(t))
	}
	big := d.addRunning("big", 9, now.Add(-10*time.Minute))
	for i := 0; i < 60 && big.killedAt.IsZero(); i++ {
		now = now.Add(tick)
		d.ladder(now)
	}
	if big.killedAt.IsZero() {
		t.Fatalf("RAM stayed critical and suspending freed nothing: the job that covers the shortfall should be stopped:\n%s", daemonLog(t))
	}
	if sysinfo.CanTrim && big.holds == 0 {
		t.Fatal("killed without trying to suspend first")
	}
	if !small.killedAt.IsZero() {
		t.Fatal("the small job was killed too")
	}
	d.finish(big.ID, 137, "evicted", "")
	r := big.receipt
	if !strings.Contains(r.Reason, "RAM critically low") || !strings.Contains(r.Reason, "frees 9.0 GB") || r.FreedRAM != 9 {
		t.Fatalf("receipt: %+v", r)
	}
}

// Admission keeps a margin: a job whose need fits with nothing to spare waits, and status says why.
// No job starts while RAM is, or just was, under the reserve.
func TestAdmissionKeepsARAMMargin(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addWaiting("ctl-port", 1.0, 90, time.Minute)
	// Reserve 8% of 32 = 2.56 GB. 3.86 GB free leaves 1.3 GB: the need fits, the 1.6 GB margin doesn't.
	d.snap.RAMAvailGB = 3.86
	d.admit(now)
	if j.State != "waiting" {
		t.Fatalf("granted with 0.3 GB to spare:\n%s", daemonLog(t))
	}
	if why := d.blocker(j); !strings.Contains(why, "needs 1.0 GB RAM, 1.3 GB free after the human's reserve") || !strings.Contains(why, "1.6 GB to spare") {
		t.Fatalf("blocker = %q", why)
	}
	if n := strings.Join(d.notes(now), "\n"); !strings.Contains(n, "GPU idle with 1 GPU job(s) waiting") || !strings.Contains(n, "needs 1.0 GB RAM") {
		t.Fatalf("status notes = %q", n)
	}
	// Room, but RAM was under the reserve 10 s ago: wait for it to settle.
	d.snap.RAMAvailGB = 12
	d.ramLowLast = now.Add(-10 * time.Second)
	d.admit(now)
	if j.State != "waiting" || !strings.Contains(d.blocker(j), "waiting 60 s for it to settle") {
		t.Fatalf("granted right after a RAM shortage (state %s, blocker %q)", j.State, d.blocker(j))
	}
	d.ramLowLast = now.Add(-2 * time.Minute)
	d.admit(now)
	if j.State != "granted" {
		t.Fatalf("not granted with room and RAM settled: %s", d.blocker(j))
	}
}

// With an overdue big job waiting for RAM, the GPU isn't left idle: a short job that fits starts
// ahead of it; a long one doesn't (it would keep the RAM the head needs).
func TestShortJobsKeepTheGPUBusyWhileTheHeadWaitsForRAM(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	head := d.addWaiting("big", 20, 240, 50*time.Minute) // overdue (max wait 45 min)
	long := d.addWaiting("long", 2, 120, 2*time.Minute)
	short := d.addWaiting("short", 2, 5, time.Minute)
	d.snap.RAMAvailGB = 12
	d.admit(now)
	if head.State != "waiting" || long.State != "waiting" || short.State != "granted" || !short.backfilled {
		t.Fatalf("head %s, long %s, short %s (backfilled %v)", head.State, long.State, short.State, short.backfilled)
	}
}

// A drive under the disk reserve: no job starts on it, and the job writing to it is suspended (not
// killed: that would free nothing) until there is room again.
func TestDiskReserve(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	cwd := t.TempDir()
	d.disks = map[string]sysinfo.Disk{"X:": {Path: "X:", TotalGB: 500, FreeGB: 4}}
	d.driveOf, d.sysDrives = map[string]string{cwd: "X:"}, map[string]bool{}
	writer := d.addRunning("writer", 1, now.Add(-5*time.Minute))
	writer.Cwd, writer.writeRate, writer.GPU = cwd, 80, false
	idle := d.addRunning("idle", 1, now.Add(-5*time.Minute))
	idle.Cwd, idle.GPU = cwd, false
	w := d.addWaiting("next", 1, 30, time.Minute)
	w.Cwd = cwd

	d.admit(now)
	if w.State != "waiting" || !strings.Contains(d.blocker(w), "disk X: has 4.0 GB free, under the 10 GB kept for the human") {
		t.Fatalf("state %s, blocker %q", w.State, d.blocker(w))
	}
	d.ladder(now) // pressure starts
	now = now.Add(25 * time.Second)
	d.ladder(now)
	if writer.held != "disk" || idle.held != "" || !writer.killedAt.IsZero() {
		t.Fatalf("writer held %q, idle held %q:\n%s", writer.held, idle.held, daemonLog(t))
	}
	d.disks["X:"] = sysinfo.Disk{Path: "X:", TotalGB: 500, FreeGB: 40}
	now = now.Add(10 * time.Second)
	d.ladder(now)
	d.ladder(now.Add(6 * time.Second))
	if writer.held != "" || !writer.suspended.IsZero() {
		t.Fatalf("writer not resumed with space back:\n%s", daemonLog(t))
	}
	d.admit(now.Add(7 * time.Second))
	if w.State != "granted" {
		t.Fatalf("not granted with space back: %s", d.blocker(w))
	}
}

// A GPU job with no --ram and no history is assumed to need gpu.default_ram_gb, not 0.5 GB.
func TestGPUJobDefaultRAMNeed(t *testing.T) {
	d := testDaemon(t)
	gpu := policy.Ticket{Project: "p", CmdHash: "x", GPU: true}
	d.fillNeeds(&gpu)
	cpu := policy.Ticket{Project: "p", CmdHash: "y"}
	d.fillNeeds(&cpu)
	declared := policy.Ticket{Project: "p", CmdHash: "z", GPU: true, RAMGB: 1}
	d.fillNeeds(&declared)
	if gpu.NeedRAM != 3 || cpu.NeedRAM != 0.5 || declared.NeedRAM != 1 {
		t.Fatalf("needs: gpu %.1f, cpu %.1f, declared %.1f", gpu.NeedRAM, cpu.NeedRAM, declared.NeedRAM)
	}
}

// 2026-11-20: OBS recording on NVENC (32% of the video encoders, 2% of 3D) held every GPU job as
// "GPU busy outside firefighter". Encoder use isn't outside GPU work; real compute still is.
func TestVideoEncodeIsNotOutsideGPUWork(t *testing.T) {
	d := testDaemon(t)
	me := os.Getpid() // a real process with a name, standing in for obs64.exe
	var obs sysinfo.ProcGPU
	obs.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_0_engtype_3D", 2.3)
	obs.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_5_engtype_VideoEncode", 15.2)
	obs.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_6_engtype_VideoEncode", 17.3)
	d.snap.ProcUtilKnown = true
	d.snap.ProcGPU = map[int]sysinfo.ProcGPU{me: obs}
	d.applyMeasurements(nil, time.Now(), time.Second)
	if out := d.busyOutside(); len(out) != 0 {
		t.Fatalf("a recorder on NVENC blocks GPU jobs: %v", out)
	}
	var cuda sysinfo.ProcGPU
	cuda.AddEngine("pid_1_luid_0x0_0x1_phys_0_eng_3_engtype_Compute_0", 60)
	d.snap.ProcGPU = map[int]sysinfo.ProcGPU{me: cuda}
	d.applyMeasurements(nil, time.Now(), time.Second)
	if out := d.busyOutside(); len(out) != 1 {
		t.Fatalf("outside CUDA work must still count: %v", out)
	}
}

// 2026-11-20: the learned reserve (9% of 31.6 GB = 2.8 GB) let free RAM fall to where Windows pages.
// reserve.min_free_gb holds a floor under it, for admission and for the ladder.
func TestMinFreeFloor(t *testing.T) {
	d := testDaemon(t)
	d.cfg.Reserve.MinFreeGB = 4
	now := time.Now()
	j := d.addWaiting("eval", 2.0, 30, time.Minute)
	// 8% of 32 = 2.56 GB reserve; 6.5 GB free would leave 3.9 after it, but the floor is 4 GB:
	// 2.5 GB left, not enough for 2 GB plus the 1.6 GB margin.
	d.snap.RAMAvailGB = 6.5
	d.admit(now)
	if j.State != "waiting" {
		t.Fatalf("granted into the floor:\n%s", daemonLog(t))
	}
	d.snap.RAMAvailGB = 8
	d.admit(now)
	if j.State != "granted" {
		t.Fatalf("not granted with room above the floor: %s", d.blocker(j))
	}
	if f := d.ramFloor(); f != 4 {
		t.Fatalf("floor %v", f)
	}
	d.cfg.Reserve.MinFreeGB = 20 // capped at 15% of RAM
	if f := d.ramFloor(); f != 32*0.15 {
		t.Fatalf("floor cap %v", f)
	}
}

// Commit nearly spent: Windows would grow the pagefile and page. A job waits even with RAM free.
func TestCommitHeadroomHoldsAdmission(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addWaiting("eval", 4.0, 30, time.Minute)
	d.snap.RAMAvailGB = 20
	d.snap.CommitLimitGB, d.snap.CommitUsedGB = 44.1, 41.1 // the freeze: 3 GB of commit left
	d.admit(now)
	if j.State != "waiting" {
		t.Fatalf("granted with 3 GB of commit left:\n%s", daemonLog(t))
	}
	d.snap.CommitUsedGB = 30
	d.admit(now)
	if j.State != "granted" {
		t.Fatalf("not granted with commit to spare: %s", d.blocker(j))
	}
}

// A container's --memory is what it may take from Windows (through the WSL VM): it counts as the
// job's RAM need even when a smaller --ram was declared (2026-11-20: --ram 6, --memory=28g).
func TestDockerMemoryLimitIsTheNeed(t *testing.T) {
	for cmd, want := range map[string]float64{
		"docker run --rm --gpus all --memory=28g img python x.py": 28,
		"docker run -m 512m img":                                  0.5,
		"docker.exe run --memory 2.5G img":                        2.5,
		"docker run -m4g img":                                     4,
		"docker build -m 4g .":                                    0,
		"python train.py --memory 8g":                             0,
		"podman run --memory=1073741824 img":                      1,
	} {
		if got := dockerMemGB(cmd); math.Abs(got-want) > 1e-9 {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
	d := testDaemon(t)
	tk := &policy.Ticket{Project: "UnomiBackend", Name: "t4sim", GPU: true, RAMGB: 6, VRAMGB: 9, Minutes: 50,
		Command: "docker run --rm --gpus all --memory=28g unomi python stages.py"}
	d.fillNeeds(tk)
	if tk.NeedRAM != 9 {
		t.Fatalf("need %.1f: declared 6 GB + half = 9 GB reserved (and the container held to it)", tk.NeedRAM)
	}
	unknown := &policy.Ticket{Project: "UnomiBackend", Name: "inspect", Command: "docker run --rm -m 28g unomi ls"}
	d.fillNeeds(unknown)
	if unknown.WantRAM != 28 || unknown.NeedRAM < 20 {
		t.Fatalf("nothing declared: the whole 28 GB limit counts (cut to what the machine can give): need %.1f want %.1f", unknown.NeedRAM, unknown.WantRAM)
	}
}

// RAM critical, the VM's numbers not read and the last reclaim a miss: the VM is asked anyway.
func TestCriticalRAMReclaimsTheVMEvenAfterAMiss(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.RAMAvailGB = 0.8
	d.vm, d.vmAt = sysinfo.VM{HostGB: 11}, now // host side only: the guest wasn't asked
	d.reclaim.misses, d.reclaim.end = 1, now.Add(-40*time.Second)
	d.ramLow = now.Add(-time.Minute)
	d.mu.Lock()
	d.ladder(now)
	on := d.reclaim.on
	d.mu.Unlock()
	if !on {
		t.Fatalf("no reclaim at 0.8 GB free with the VM holding 11 GB:\n%s", daemonLog(t))
	}
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		d.mu.Lock()
		on = d.reclaim.on
		d.mu.Unlock()
		if !on {
			break
		}
	}
	if b, _ := os.ReadFile(os.Getenv("FIREFIGHTER_FAKE_FILE") + ".reclaim"); len(b) == 0 {
		t.Fatal("the VM was never asked")
	}
}

// 2026-11-20: a job's script (run_matrix.py) ran `docker run --memory=16g`; the container counted
// as the human's load and the ladder couldn't act on it. A container a job's own docker CLI starts
// is the job's, and it is held to the job's reservation.
func TestJobStartedContainerIsTheJobs(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("t4sim iter1 stages", 0.1, now.Add(-time.Minute))
	j.NeedRAM = 6
	other := d.addRunning("unrelated", 1, now.Add(-time.Minute))
	cs := []sysinfo.Container{{ID: "1a9a7107362a", Name: "keen_grothendieck", MemGB: 1.3, Created: now.Add(-50 * time.Second)}}
	since := map[string]time.Time{j.ID: now.Add(-55 * time.Second)} // its docker run started just before
	lines := map[string][]string{j.ID: {"docker run --rm --gpus all --cpus=4 --memory=16g --shm-size=8g img python stage.py"}}
	d.adoptContainers(cs, nil, nil) // just after a restart: the job's processes aren't measured yet
	if j.container != "" {
		t.Fatal("assigned with no CLI seen")
	}
	d.adoptContainers(cs, lines, since) // the next look finds the job's docker run
	if j.container != "1a9a7107362a" || other.container != "" {
		t.Fatalf("container not given to the job that started it: %q / %q\n%s", j.container, other.container, daemonLog(t))
	}
	select {
	case args := <-d.dockerQ:
		if got := strings.Join(args, " "); got != "docker update --memory 6144m --memory-swap 6144m 1a9a7107362a" {
			t.Fatalf("docker call %q", got)
		}
	default:
		t.Fatal("the container was not held to the job's reservation")
	}
	// Two jobs with a live docker run and nothing naming the container: no guess.
	k := d.addRunning("second", 0.1, now)
	cs = append(cs, sysinfo.Container{ID: "bbbbbbbbbbbb", Name: "other_name", MemGB: 1, Created: now})
	d.adoptContainers(cs, map[string][]string{j.ID: {"docker run img"}, k.ID: {"docker run img2"}}, map[string]time.Time{j.ID: now.Add(-time.Minute), k.ID: now.Add(-time.Second)})
	if k.container != "" || j.container != "1a9a7107362a" {
		t.Fatalf("ambiguous container was assigned: %q %q", j.container, k.container)
	}
	if !isDockerRun(`"C:\Program Files\Docker\docker.exe" run --rm x`) || isDockerRun("docker ps") {
		t.Fatal("isDockerRun")
	}
}

// 11/20: an agent cancelled and resubmitted its job 8 times to change --vram/--ram. The wait line
// says what declaration would start it, and amend changes it in place.
func TestNearMissHint(t *testing.T) {
	if h := nearMiss("335c69a6", "vram", 9, 9, 8.86); !strings.Contains(h, "would start now with --vram 8.8 (declared 9.0): firefighter amend 335c69a6 --vram 8.8") {
		t.Fatalf("hint %q", h)
	}
	if h := nearMiss("x", "ram", 13.3, 14, 11); !strings.Contains(h, "--ram 11.0 (declared 14.0)") {
		t.Fatalf("a declaration cut to the machine's most still gets the hint: %q", h)
	}
	if nearMiss("x", "vram", 9, 0, 8.8) != "" || nearMiss("x", "vram", 12, 9, 8.8) != "" || nearMiss("x", "ram", 14, 14, 3) != "" {
		t.Fatal("a hint only when the job's own declaration sets the need and the miss is small")
	}
}

// 11/20: the state flipped game/active/present every 1-3 minutes while TF2 was fullscreen. Stepping
// down needs the lower state in 2 of 3 readings 10 s apart; stepping up is immediate.
func TestUserStateHysteresis(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.user = "game"
	if s := d.steadyUser("active", now); s != "game" {
		t.Fatalf("one reading stepped down: %s", s)
	}
	if s := d.steadyUser("active", now.Add(3*time.Second)); s != "game" {
		t.Fatalf("readings closer than 10 s count once: %s", s)
	}
	if s := d.steadyUser("active", now.Add(11*time.Second)); s != "active" {
		t.Fatalf("two readings 10 s apart should step down: %s", s)
	}
	d.user = "active"
	if s := d.steadyUser("game", now.Add(12*time.Second)); s != "game" {
		t.Fatalf("a step up must be immediate: %s", s)
	}
	// A fullscreen game that leaves fullscreen but stays in front is still the game.
	d.user = "present"
	d.snap.Idle = 2
	d.snap.Foreground, d.snap.ForegroundApp = "tf_win64.exe", "tf_win64.exe"
	d.senseUser(now)
	if d.user != "game" {
		t.Fatalf("fullscreen game not detected: %s", d.user)
	}
	d.snap.Foreground = "" // a menu: not fullscreen for a moment
	for i := 1; i <= 6; i++ {
		d.senseUser(now.Add(time.Duration(i*10) * time.Second))
		if d.user != "game" {
			t.Fatalf("game dropped at +%ds while it stayed in front: %s", i*10, d.user)
		}
	}
	d.snap.ForegroundApp = "code.exe" // the human alt-tabs away for good
	for i := 7; i <= 12; i++ {
		d.senseUser(now.Add(time.Duration(i*10) * time.Second))
	}
	if d.user == "game" {
		t.Fatal("game held after the game left the front")
	}
}

// 11/20: "starts ~8:31 AM" stayed up while the blocker went RAM, then VRAM, then OBS. The estimate
// now follows what blocks the job: firefighter's own jobs (the schedule knows when they end), a
// back-off (ends at a known time), or something outside it (no forecast).
func TestETAFollowsTheBlocker(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.RAMAvailGB = 20
	j := d.addWaiting("t4sim", 2, 30, time.Minute)
	j.NeedVRAM = 9
	d.snap.GPUs[0].UsedGB = 12 // 4 GB free, held by something outside firefighter
	if got := d.startsIn(j, 7, now); got != -1 {
		t.Fatalf("VRAM held outside firefighter: %v, want no forecast", got)
	}
	r := d.addRunning("other job", 1, now.Add(-time.Minute))
	r.vramGB, r.NeedVRAM = 8, 8 // now firefighter's own job holds it: the schedule's estimate holds
	if got := d.startsIn(j, 7, now); got != 7 {
		t.Fatalf("VRAM held by a running job: %v, want the schedule's 7", got)
	}
	d.backoff = map[string]backoff{j.ID: {n: 3, at: now.Add(-time.Minute), why: "RAM"}}
	if got := d.startsIn(j, 1, now); got < 2.9 || got > 3.1 {
		t.Fatalf("back-off: %v, want the 3 minutes it has left", got)
	}
	if Approx("unknown") != "unknown" || Approx("8:31 AM") != "~8:31 AM" {
		t.Fatal("Approx")
	}
}

// 11/20: a stopped job's wait line said only "stopped for the human a moment ago: waiting a while
// before trying again". It now says what it was stopped for, with the numbers, and when it retries.
func TestBackoffLineSaysWhy(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addWaiting("cpu-scaling", 2, 30, time.Minute)
	d.snap.RAMTotalGB, d.vm = 31.6, sysinfo.VM{HostGB: 12.2, CacheGB: 10.2, Guest: true}
	d.res.RAMPct = 10
	d.cfg.Reserve.MinFreeGB = 4
	why := d.ramWhy(2.7, d.ramFloor())
	for _, want := range []string{"2.7 GB free", "1.3 GB under the 4.0 GB floor", "reserve.min_free_gb", "10% reserve is 3.2 GB", "VM holds 12.2 GB, 10.2 GB of it file cache"} {
		if !strings.Contains(why, want) {
			t.Fatalf("reason %q lacks %q", why, want)
		}
	}
	d.backoff = map[string]backoff{j.ID: {n: 2, at: now.Add(-30 * time.Second), why: why}}
	line := d.blockedByHuman(j, now)
	if !strings.Contains(line, "stopped 30s ago (RAM: 2.7 GB free") || !strings.Contains(line, "tries again in 1m30s") {
		t.Fatalf("wait line %q", line)
	}
}

// Review 11/20 (GPT): the human's container, already running when a job's docker run starts, must
// never be guessed to be the job's (it would be capped, paused and killed with it).
func TestHumansContainerIsNotGuessed(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("pulls an image", 1, now.Add(-time.Minute))
	db := sysinfo.Container{ID: "dbdbdbdbdbdb", Name: "postgres", MemGB: 2, Created: now.Add(-48 * time.Hour)}
	d.adoptContainers([]sysinfo.Container{db}, map[string][]string{j.ID: {"docker run img"}}, map[string]time.Time{j.ID: now.Add(-30 * time.Second)})
	if j.container != "" {
		t.Fatalf("the human's container was given to the job: %q", j.container)
	}
	// unknown creation time: no guess either
	d.seenCtr = nil
	db.Created = time.Time{}
	d.adoptContainers([]sysinfo.Container{db}, map[string][]string{j.ID: {"docker run img"}}, map[string]time.Time{j.ID: now.Add(-30 * time.Second)})
	if j.container != "" {
		t.Fatal("a container of unknown age was given to the job")
	}
}

// Review round 2 (Fable): two new containers in one scan, either order: neither is guessed. And a
// --name that only shares a prefix with the human's container is no match.
func TestContainerGuessNeedsOneNewAndExactName(t *testing.T) {
	for _, flip := range []bool{false, true} {
		d := testDaemon(t)
		now := time.Now()
		j := d.addRunning("job", 1, now.Add(-time.Minute))
		start := map[string]time.Time{j.ID: now.Add(-30 * time.Second)}
		a := sysinfo.Container{ID: "aaaaaaaaaaaa", Name: "jobs", Created: now.Add(-20 * time.Second)}
		b := sysinfo.Container{ID: "bbbbbbbbbbbb", Name: "humans", Created: now.Add(-10 * time.Second)}
		cs := []sysinfo.Container{a, b}
		if flip {
			cs = []sysinfo.Container{b, a}
		}
		d.adoptContainers(cs, map[string][]string{j.ID: {"docker run img"}}, start)
		if j.container != "" {
			t.Fatalf("flip=%v: one of two new containers was guessed: %q", flip, j.container)
		}
	}
	if namedIn("docker run --name app-test img", "app") || !namedIn(`docker run --name "app" img`, "app") || !namedIn("docker run --name=app img", "app") {
		t.Fatal("namedIn")
	}
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("job", 1, now.Add(-time.Minute))
	app := sysinfo.Container{ID: "cccccccccccc", Name: "app", Created: now.Add(-24 * time.Hour)}
	d.adoptContainers([]sysinfo.Container{app}, map[string][]string{j.ID: {"docker run --name app img"}}, map[string]time.Time{j.ID: now.Add(-30 * time.Second)})
	if j.container != "" {
		t.Fatal("a container older than the job's docker run was taken by name")
	}
}

// 11/20: agents manage the storage the human allots them. A project whose space holds more than
// it claimed starts no new jobs (except --quick ones, which may be its cleanup) until it deletes
// files or claims more.
func TestStorageOverClaimHoldsNewJobs(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.RAMAvailGB = 20
	j := d.addWaiting("train", 1, 30, time.Minute)
	j.Project = "alpha"
	d.storeRep = &storage.Report{GB: 50, Spaces: []storage.Space{{Project: "alpha", GB: 10, UsedGB: 12.4, Over: true, Path: `C:\s\alpha`}}}
	if b := d.blockerOf(j); !strings.Contains(b, "alpha's space holds 12.4 GB, over its 10.0 GB claim") || !strings.Contains(b, "storage claim --gb 14") {
		t.Fatalf("blocker %q", b)
	}
	if got := d.startsIn(j, 1, now); got != -1 {
		t.Fatalf("start estimate %v, want unknown", got)
	}
	if n := d.notes(now); !strings.Contains(strings.Join(n, "|"), "over its 10.0 GB claim") {
		t.Fatalf("notes %v", n)
	}
	j.Quick = true
	if w := d.storageWhy(j); w == "" {
		t.Fatal("a --quick job got past the storage hold (review round 2: its lease can be overrun)")
	}
	j.Quick, j.Project = false, "beta"
	if w := d.storageWhy(j); w != "" {
		t.Fatalf("another project is held: %q", w)
	}
}

// Review round 2 (GPT): docker options end at the image; -d is refused by run.
func TestDockerRunOptions(t *testing.T) {
	if got := dockerMemGB("docker run --rm -e X=1 img python app.py --memory=64m"); got != 0 {
		t.Fatalf("a --memory after the image was read as the container's: %v", got)
	}
	if got := dockerMemGB("docker run --rm -m 2g img"); got != 2 {
		t.Fatalf("-m 2g: %v", got)
	}
	for cmd, want := range map[string]bool{
		"docker run -d img": true, "docker run -dit img": true, "docker run --detach img": true,
		"docker run --rm -it img": false, "docker run -e -d=1 img": false, "docker run img sh -c 'x -d'": false,
		`"C:\Program Files\Docker\docker.exe" run -d img`: true, "docker ps -d": false,
	} {
		if DockerDetached(cmd) != want {
			t.Errorf("DockerDetached(%q) != %v", cmd, want)
		}
	}
}
