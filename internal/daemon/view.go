package daemon

// Job and machine status, forecasts, and blocker explanations.

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/harvest"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// get returns a ticket. ?wait=N long-polls up to N seconds for a change of state from ?state=.
func (d *Daemon) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait, _ := strconv.ParseFloat(r.URL.Query().Get("wait"), 64)
	from := r.URL.Query().Get("state")
	deadline := time.Now().Add(time.Duration(math.Min(wait, 60) * float64(time.Second)))
	var was *Declared
	for {
		d.mu.Lock()
		j, ok := d.jobs[id]
		if !ok {
			rec := d.st.Receipts[id]
			d.mu.Unlock()
			if rec != nil {
				reply(w, View{ID: id, State: "done", Receipt: rec})
				return
			}
			fail(w, 404, "no such job "+id)
			return
		}
		v := d.view(j)
		ch := d.notify
		d.mu.Unlock()
		// An amend or re-bid also ends the wait: the client must hold the new declarations at once,
		// or a guard restart in the next seconds would re-register the old ones (review 11/20).
		amended := was != nil && v.Declared != nil && *v.Declared != *was
		if was == nil {
			was = v.Declared
		}
		if from == "" || v.State != from || amended || !time.Now().Before(deadline) {
			reply(w, v)
			return
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ch:
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

// clock formats a time the way people in the US read it: "3:04 PM".
func clock(t time.Time) string { return t.Format("3:04 PM") }

// ---- views --------------------------------------------------------------------------------------

type View struct {
	ID       string  `json:"id"`
	Name     string  `json:"name,omitempty"`
	Project  string  `json:"project,omitempty"`
	State    string  `json:"state"`                // waiting | granted | running | done
	RAMNow   float64 `json:"ram_now_gb,omitempty"` // running: the RAM it holds now, as measured
	Band     int     `json:"band,omitempty"`
	Position int     `json:"position,omitempty"` // in its line (GPU or CPU), 1 = next
	ETA      string  `json:"eta,omitempty"`      // when it should start (waiting) or end (running)
	Why      string  `json:"why,omitempty"`      // what it's waiting for, or why it's paused
	Paused   bool    `json:"paused,omitempty"`
	Checkpt  bool    `json:"checkpoint,omitempty"`
	StopIn   float64 `json:"stop_in_s,omitempty"` // seconds until a checkpointing job is stopped hard
	StartsIn float64 `json:"starts_in_min,omitempty"`
	Tier     string  `json:"tier,omitempty"` // spot | normal | urgent
	Budget   float64 `json:"budget_left"`    // project's machine-minutes left
	Pred     float64 `json:"predicted_minutes,omitempty"`
	GPU      bool    `json:"gpu,omitempty"`
	NeedRAM  float64 `json:"need_ram_gb,omitempty"` // what admission counts (declared, learned, or the machine's most)
	NeedVRAM float64 `json:"need_vram_gb,omitempty"`
	// LearnedRAM: what its past runs alone point to, when that is more than what is counted.
	LearnedRAM float64 `json:"learned_ram_gb,omitempty"`
	Background bool    `json:"background,omitempty"` // its project is a bot: it waits behind project work
	// Declared: the job's declarations now (after any amend or re-bid). A client that has to
	// register its job again (the guard restarted) sends these, not the ones it started with.
	Declared *Declared     `json:"declared,omitempty"`
	Receipt  *Receipt      `json:"receipt,omitempty"`
	Quote    *policy.Quote `json:"quote,omitempty"`
	Market   string        `json:"market,omitempty"`     // the quote in one line
	Device   *int          `json:"gpu_device,omitempty"` // the GPU it was placed on, in its vendor's numbering
	Vendor   string        `json:"gpu_vendor,omitempty"` // nvidia | amd | intel
	PauseF   string        `json:"pause_file,omitempty"`
	PausedF  string        `json:"paused_file,omitempty"`
	CheckF   string        `json:"checkpoint_file,omitempty"`
}

func (d *Daemon) view(j *job) View {
	now := time.Now()
	var decision Decision
	if j.State == "waiting" {
		decision = d.waitingDecision(j, now)
	}
	return d.viewWithDecision(j, now, decision)
}

func (d *Daemon) viewWithDecision(j *job, now time.Time, decision Decision) View {
	a := d.peek(j.Project, now)
	v := View{ID: j.ID, Name: j.Name, Project: j.Project, State: j.State, Band: j.Band, Tier: tierOf(j.Ticket),
		Declared: &Declared{RAMGB: j.RAMGB, VRAMGB: j.VRAMGB, Minutes: j.Minutes, Bid: j.Bid, Spot: j.Spot},
		Budget:   round1(math.Max(-1e9, math.Min(1e9, a.Left(d.cfg, d.cfg.Allowance(j.Project))))), Pred: round1(j.PredMinutes),
		GPU: j.GPU, NeedRAM: round1(j.NeedRAM), NeedVRAM: round1(j.NeedVRAM), Background: j.Background,
		LearnedRAM: func() float64 {
			if j.LearnedRAM > j.NeedRAM+0.05 {
				return j.LearnedRAM
			}
			return 0
		}(),
		Receipt: j.receipt, Paused: !j.pausedAt.IsZero(), Checkpt: !j.checkpoint.IsZero(), Why: j.reason,
		StopIn: func() float64 {
			if j.checkpoint.IsZero() {
				return 0
			}
			return math.Max(0.1, d.cfg.Ladder.GraceSeconds-time.Since(j.checkpoint).Seconds())
		}(),
		PauseF: PauseFile(j.ID), PausedF: PausedFile(j.ID), CheckF: CheckpointFile(j.ID)}
	if j.device >= 0 && j.device < len(d.snap.GPUs) {
		g := d.snap.GPUs[j.device]
		dev := g.Index
		v.Device, v.Vendor = &dev, g.Vendor
	}
	switch j.State {
	case "waiting":
		// Apply the admission hold to the market's schedule before showing either estimate.
		q := d.quote(*j.Ticket, now)
		for index := range q.Menu {
			q.Menu[index].StartsIn = decision.startsIn(q.Menu[index].StartsIn)
		}
		own, _ := q.Tier(tierOf(j.Ticket))
		v.Position, v.Why = d.position(j, now), blockerText(j, decision.Text)
		_ = own
		v.StartsIn, v.Quote = own.StartsIn, &q
		v.ETA = "unknown"
		if v.StartsIn >= 0 {
			v.ETA = clock(now.Add(time.Duration(v.StartsIn * float64(time.Minute))))
			q.Do = policy.Advise(q, j.Bid >= policy.BidUrgent)
			v.Market = q.Line()
		} else {
			// The schedule's start times don't hold while something outside it blocks the job:
			// no market line with a start in it, and the advice says what it waits for (review 11/20).
			q.Do = "keep working meanwhile; it starts when this clears: " + v.Why
		}
	case "running":
		v.RAMNow = round1(j.ramGB)
		left := policy.Remaining(j.PredMinutes+j.extended, j.leaseUsed(now).Minutes())
		v.ETA = clock(now.Add(time.Duration(left * float64(time.Minute))))
	case "granted":
		v.ETA = clock(now.Add(time.Duration(math.Max(1, j.PredMinutes+j.extended) * float64(time.Minute))))
	}
	return v
}

// Approx is a start or end time for display: "~8:31 AM", or "unknown" when there is no forecast.
func Approx(eta string) string {
	if eta == "" || eta == "unknown" {
		return "unknown"
	}
	return "~" + eta
}

func (d *Daemon) startsIn(j *job, sim float64, now time.Time) float64 {
	return d.waitingDecision(j, now).startsIn(sim)
}

func (d *Daemon) blocker(j *job) string {
	return blockerText(j, d.blockerOf(j))
}

func (d *Daemon) blockerOf(j *job) string {
	return d.waitingDecision(j, time.Now()).Text
}

func blockerText(j *job, why string) string {
	switch {
	case j.WantRAM > 0:
		src := "its past runs point to"
		if j.RAMGB >= j.WantRAM-0.05 {
			src = "it declared"
		}
		why += fmt.Sprintf(" (%s %.1f GB of RAM, more than this machine ever has free for a job: counted as %.1f GB, so it starts when the machine is at its emptiest)", src, j.WantRAM, j.NeedRAM)
	case j.RAMGB > 0 && j.LearnedRAM > j.RAMGB+0.05 && strings.Contains(why, "GB RAM"):
		why += fmt.Sprintf(" (counted at its declared --ram %.1f GB; its past runs point to %.1f GB, so if it grows past that it is suspended for the human, not killed)", j.RAMGB, j.LearnedRAM)
	}
	if j.Band == policy.BandSpot {
		why += " [spot: over budget or --spot, so it goes after in-budget jobs and is preempted first; it still runs whenever there is room]"
	}
	return why
}

// waitRAMWhy says why a job waits on RAM with the arithmetic shown: what it needs (and the margin kept
// on top), what is free for jobs, and what that figure is made of. ctx.ram already has the jobs
// admitted earlier in this pass taken off; those are named too.
func (d *Daemon) waitRAMWhy(j *job, ctx *admitCtx) string {
	parts := d.ramParts()
	need := fmt.Sprintf("needs %.1f GB RAM", j.NeedRAM)
	if ctx.margin >= 0.05 {
		need = fmt.Sprintf("needs %.1f GB RAM + %.1f GB margin (reserve.ram_margin_pct)", j.NeedRAM, ctx.margin)
	}
	free := math.Max(0, ctx.ram)
	why := fmt.Sprintf("%s; %.1f GB free for jobs = %s", need, free, parts)
	if passed := parts.left() - ctx.ram; passed >= 0.05 {
		why += fmt.Sprintf(" - %.1f GB for jobs starting ahead of it", passed)
	}
	return why + d.vmWaitNote(ctx.now)
}

// vmWaitNote adds where the RAM went when the local Linux VM holds it: a job waiting on RAM should say
// what would let it start.
func (d *Daemon) vmWaitNote(now time.Time) string {
	switch cache := d.reclaimable(now); {
	case d.reclaim.on:
		return "; the WSL/Docker VM is giving its file cache back now"
	case cache >= reclaimMinGB:
		return fmt.Sprintf("; the WSL/Docker VM holds %.1f GB, %.1f GB of it file cache that is reclaimed next", d.vm.HostGB, cache)
	case d.vm.HostGB >= 4:
		return fmt.Sprintf("; the WSL/Docker VM holds %.1f GB", d.vm.HostGB)
	}
	return ""
}

func (d *Daemon) blockedByHuman(j *job, now time.Time) string {
	decision := d.waitingDecision(j, now)
	switch decision.Kind {
	case blockBackoff, blockHuman, blockNetwork:
		return decision.Text
	}
	return ""
}

// notes are the few lines `status` leads with when something is holding work back: an idle GPU
// with jobs waiting (and what they wait for), a reclaim under way, jobs suspended for RAM or disk,
// a drive under the reserve.
func (d *Daemon) notes(now time.Time) []string {
	var out []string
	busy, waiting, first := 0, 0, (*job)(nil)
	for _, j := range d.jobs {
		if j.GPU && (j.State == "granted" || (j.State == "running" && j.suspended.IsZero())) {
			busy++
		}
	}
	for _, t := range d.waiting() {
		if t.GPU {
			if waiting++; first == nil {
				first = d.jobs[t.ID]
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if _, err := os.Stat(exe); err != nil {
			// 2026-11-13, 11-20 and 11-21: Microsoft Defender's machine-learning rules quarantined the
			// unsigned firefighter.exe while the guard ran from it; every new `firefighter` call then
			// failed while this guard still answered.
			out = append(out, fmt.Sprintf("this guard's program %s is gone from disk (antivirus quarantine? see Windows Security > Protection history); new firefighter commands fail until it is restored or rebuilt", exe))
		}
	}
	if h := d.cfg.Harvester; h.Start != "" && !harvest.Approved(config.Home(), h.Start) {
		out = append(out, "harvester: its commands in firefighter.toml aren't approved, so they don't run; check them, then run `firefighter harvest approve` from your own terminal")
	}
	if busy == 0 && waiting > 0 {
		out = append(out, fmt.Sprintf("GPU idle with %d GPU job(s) waiting. Next in line, %s: %s", waiting, first.Name, d.blocker(first)))
	}
	if d.reclaim.on {
		out = append(out, fmt.Sprintf("RAM: the WSL/Docker VM is giving back its file cache (started %.0f s ago); no job is stopped for it", now.Sub(d.reclaim.at).Seconds()))
	}
	for _, j := range d.running() {
		if j.held != "" {
			out = append(out, fmt.Sprintf("%s is suspended, not stopped: %s", j.Name, j.reason))
		}
	}
	for dr, dk := range d.disks {
		if d.cfg.Reserve.DiskFreeGB > 0 && dk.FreeGB < d.diskFloor(dk) {
			out = append(out, fmt.Sprintf("disk %s has %.1f GB free, under the %.0f GB kept for the human: no job starts on it, and jobs writing to it are suspended", dr, dk.FreeGB, d.diskFloor(dk)))
		}
	}
	if r := d.storeRep; r != nil {
		for _, sp := range r.Spaces {
			if sp.Over && sp.GB == 0 {
				out = append(out, fmt.Sprintf("storage: %s holds %.1f GB in %s without a claim: its new jobs wait until it's claimed or deleted", sp.Project, sp.UsedGB, sp.Path))
			} else if sp.Over {
				out = append(out, fmt.Sprintf("storage: %s's space holds %.1f GB, over its %.1f GB claim: its new jobs wait until it deletes files or claims more", sp.Project, sp.UsedGB, sp.GB))
			}
		}
		if r.GrantedGB > r.GB+0.05 {
			out = append(out, fmt.Sprintf("storage: %.1f GB is claimed but the allotment is now %.1f GB: the spaces must shrink", r.GrantedGB, r.GB))
		}
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) busyOutside() []string {
	var out []string
	for n, u := range d.foreign {
		if u >= d.cfg.Reserve.ForeignGPUPct {
			out = append(out, fmt.Sprintf("%s %.0f%%", cleanText(n, 60), u)) // a program names itself
		}
	}
	return out
}

// Declared is what a job declared, as the guard holds it now.
type Declared struct {
	RAMGB   float64 `json:"ram_gb"`
	VRAMGB  float64 `json:"vram_gb"`
	Minutes float64 `json:"minutes"`
	Bid     int     `json:"bid"`
	Spot    bool    `json:"spot,omitempty"`
}

// Status is the whole picture, for `firefighter status`, MCP and STATUS.md.
type Status struct {
	Time      time.Time               `json:"time"`
	User      string                  `json:"user"`
	UserWhy   string                  `json:"user_why,omitempty"`
	Reserve   map[string]float64      `json:"reserve"`
	Machine   map[string]float64      `json:"machine"`
	GPUs      []sysinfo.GPU           `json:"gpus"`
	Running   []View                  `json:"running"`
	Waiting   []View                  `json:"waiting"`
	Budgets   map[string]float64      `json:"budgets"`
	Plans     []*Plan                 `json:"plans"`
	Foreign   map[string]float64      `json:"other_gpu_users"`
	Docker    []sysinfo.Container     `json:"containers"`
	Policy    map[string]string       `json:"policy"`
	ConfigErr string                  `json:"config_error,omitempty"`
	Recent    []*Receipt              `json:"recent"`
	Notes     []string                `json:"notes,omitempty"`    // what is holding work back right now, in plain words
	Limits    []string                `json:"limits,omitempty"`   // the binding constraint of each line, with its numbers
	Idle      []string                `json:"idle,omitempty"`     // resources idle in the last hour while jobs for them waited
	Version   string                  `json:"version"`            // the guard's version
	VM        *sysinfo.VM             `json:"vm,omitempty"`       // the WSL2/Docker VM's memory, when one runs
	Disks     []sysinfo.Disk          `json:"disks,omitempty"`    // drives jobs and the system use
	Forecast  *Forecast               `json:"forecast,omitempty"` // what the human and each command are expected to use
	Market    map[string]policy.Quote `json:"market"`             // posted prices and odds for a typical 30-minute job
	Storage   *storage.Report         `json:"storage,omitempty"`  // agents' storage spaces (measured every 2 minutes)
}

func (d *Daemon) status() Status {
	now := time.Now()
	vt, vu := d.snap.VRAMTotal()
	s := Status{Time: now, User: d.user, Plans: d.st.Plans, Foreign: d.foreign, Docker: d.outsideContainers(), GPUs: d.snap.GPUs,
		ConfigErr: d.cfgErr, Budgets: map[string]float64{},
		Reserve: map[string]float64{"cpus": d.res.CPUs, "ram_pct": d.res.RAMPct, "vram_gb": d.res.VRAMGB},
		Machine: map[string]float64{"cpus": float64(d.snap.CPUs), "cpu_busy": round1(d.snap.CPUBusy), "user_cpus": round1(d.busyUser),
			"ram_total_gb": round1(d.snap.RAMTotalGB), "ram_avail_gb": round1(d.snap.RAMAvailGB),
			"net_mbps": round1(d.snap.NetMbps), "net_human_mbps": round1(d.netHuman), "net_link_mbps": round1(d.netCap),
			"vram_total_gb": round1(vt), "vram_used_gb": round1(vu), "gpu_util": d.snap.GPUUtil(), "idle_s": math.Round(d.snap.Idle)},
		Policy: map[string]string{"preset": d.cfg.Preset, "order": d.cfg.Order.Policy, "pricing": d.cfg.Pricing.Policy, "budget": d.cfg.Budget.Policy}}
	if d.user == "game" {
		s.UserWhy = d.gameWhy
	}
	if d.vm.HostGB > 0 {
		vm := d.vm
		s.VM = &vm
		s.Machine["ram_vm_gb"], s.Machine["ram_reclaimable_gb"] = round1(vm.HostGB), round1(d.reclaimable(now))
	}
	for _, dk := range d.disks {
		dk.TotalGB, dk.FreeGB = round1(dk.TotalGB), round1(dk.FreeGB)
		s.Disks = append(s.Disks, dk)
	}
	sort.Slice(s.Disks, func(a, b int) bool { return s.Disks[a].Path < s.Disks[b].Path })
	s.Reserve["disk_free_gb"] = d.cfg.Reserve.DiskFreeGB
	s.Storage = d.storeRep
	s.Notes, s.Version = d.notes(now), d.Version
	s.Limits, s.Idle = d.limits(now), d.utilLines()
	s.Machine["ram_for_jobs_gb"], s.Machine["ram_reserve_gb"] = round1(math.Max(0, d.ramParts().left())), round1(d.ramFloor())
	if d.updateNote != "" {
		s.Notes = append(s.Notes, d.updateNote)
	}
	s.Forecast = d.forecast(now)
	s.Market = map[string]policy.Quote{}
	for _, gpu := range []bool{true, false} {
		p := policy.Ticket{Project: "default", GPU: gpu, Minutes: 30, Bid: policy.BidNormal}
		d.fillNeeds(&p)
		s.Market[map[bool]string{true: "gpu", false: "cpu"}[gpu]] = d.quote(p, now)
	}
	for _, j := range d.running() {
		s.Running = append(s.Running, d.view(j))
	}
	for _, j := range d.jobs {
		if j.State == "granted" { // starting: it holds its turn already
			s.Running = append(s.Running, d.view(j))
		}
	}
	waiting := d.waiting()
	ctx := d.newAdmitCtx(now)
	decisions := make(map[string]Decision, len(waiting))
	for _, ticket := range d.gateOrder(waiting) {
		decisions[ticket.ID] = d.gate(d.jobs[ticket.ID], ctx)
	}
	for _, ticket := range waiting {
		s.Waiting = append(s.Waiting, d.viewWithDecision(d.jobs[ticket.ID], now, decisions[ticket.ID]))
	}
	for p := range d.st.Accounts {
		a := d.peek(p, now)
		s.Budgets[p] = round1(math.Max(-1e9, math.Min(1e9, a.Left(d.cfg, d.cfg.Allowance(p)))))
	}
	for _, r := range d.st.Receipts {
		if now.Sub(r.Ended) < 2*time.Hour {
			s.Recent = append(s.Recent, r)
		}
	}
	return s
}

// writeStatus mirrors the status to STATUS.md for agents that read files instead of calling tools.
func (d *Daemon) writeStatus() {
	s := d.status()
	var b strings.Builder
	fmt.Fprintf(&b, "# firefighter status, %s\n\nRewritten every 15 s. Older than a minute means the guard is down: run `firefighter daemon`.\n\n", s.Time.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Human: **%s** %s. Reserve kept for them: %.0f CPUs, %.0f%% RAM, %.1f GB VRAM.\n\n", s.User, s.UserWhy, s.Reserve["cpus"], s.Reserve["ram_pct"], s.Reserve["vram_gb"])
	for _, n := range s.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	if len(s.Notes) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "| Resource | Use |\n|---|---|\n| CPU | %.1f of %.0f busy (%.1f by the human and other programs) |\n| RAM | %.1f of %.1f GB free |\n| GPU | %.0f%% |\n| VRAM | %.1f of %.1f GB used |\n\n",
		s.Machine["cpu_busy"], s.Machine["cpus"], s.Machine["user_cpus"], s.Machine["ram_avail_gb"], s.Machine["ram_total_gb"], s.Machine["gpu_util"], s.Machine["vram_used_gb"], s.Machine["vram_total_gb"])
	b.WriteString("## Running (busy until)\n\n| Job | Project | Until | Note |\n|---|---|---|---|\n")
	for _, v := range s.Running {
		fmt.Fprintf(&b, "| %s | %s | ~%s | %s |\n", v.Name, v.Project, v.ETA, v.Why)
	}
	b.WriteString("\n## Waiting\n\n| # | Job | Project | Starts | Waiting for |\n|---|---|---|---|---|\n")
	for _, v := range s.Waiting {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", v.Position, v.Name, v.Project, Approx(v.ETA), v.Why)
	}
	b.WriteString("\n## Planned\n\n")
	for _, p := range s.Plans {
		fmt.Fprintf(&b, "- %s: %s, %s, ~%.0f min, GPU %.0f%%, %.1f GB VRAM, %.1f GB RAM\n", p.Project, p.What, p.When, p.Minutes, p.GPUPct, p.VRAMGB, p.RAMGB)
	}
	_ = os.WriteFile(filepath.Join(config.Home(), "STATUS.md"), []byte(b.String()), 0o600)
}

// nearMiss: when a job misses by a little and its own declaration is what sets the need, the
// declaration that would start it now (11/20: an agent resubmitted 8 times, missing by 0.1-3 GB).
func nearMiss(id, flag string, need, declared, fits float64) string {
	fits = math.Floor(fits*10) / 10
	if declared <= 0 || fits <= 0 || declared+0.05 < need || need-fits > math.Max(1, need*0.35) {
		return ""
	}
	return fmt.Sprintf("; would start now with --%s %.1f (declared %.1f): firefighter amend %s --%s %.1f", flag, fits, declared, id, flag, fits)
}

// limitName names what holds a job, for status's binding-constraint lines.
func (d *Daemon) limitName(k BlockKind) string {
	switch k {
	case blockRAM:
		if d.ramParts().commitBound() {
			return "commit"
		}
		return "physical RAM"
	case blockVRAM:
		return "VRAM"
	case blockGPUPacking, blockGPUSlot:
		return "GPU packing"
	case blockOutsideGPU:
		return "GPU in use outside firefighter"
	case blockHuman:
		return "the human"
	case blockCPU:
		return "CPU reserve"
	case blockBackfill:
		return "backfill (a bigger job waits first)"
	case blockDisk, blockStorage:
		return "disk"
	case blockNetwork:
		return "network"
	case blockBackoff:
		return "backoff"
	case blockPacing:
		return "pacing"
	case blockPriority:
		return "a higher bid waiting"
	}
	return "other"
}

// limits says, for the RAM jobs can take and for the head of each line, what binds right now.
func (d *Daemon) limits(now time.Time) []string {
	r := d.ramParts()
	how := "basic use"
	if !d.basicAuto(d.user) {
		how = "fixed (reserve.auto off or a game)"
	}
	out := []string{fmt.Sprintf("RAM for jobs %.1f GB = %s [reserve %.1f GB for the human while %s: %s]", math.Max(0, r.left()), r, d.ramFloor(), d.user, how)}
	seen := map[bool]bool{}
	for _, t := range d.waiting() {
		if seen[t.GPU] {
			continue
		}
		seen[t.GPU] = true
		j := d.jobs[t.ID]
		dec := d.waitingDecision(j, now)
		line := map[bool]string{true: "GPU", false: "CPU"}[t.GPU]
		if dec.Kind == blockNone {
			out = append(out, fmt.Sprintf("%s line: %s starts next", line, j.Name))
			continue
		}
		out = append(out, fmt.Sprintf("%s line: %s held by %s: %s", line, j.Name, d.limitName(dec.Kind), dec.Text))
	}
	return out
}

// position is a waiting job's real place in its line (GPU or CPU), 1 = next: the order admission
// walks, not a quote's guess for a new job (which priced every job at bid 5, so two jobs could
// both show #2 and an amend seemed to move one; Josh 11/22).
func (d *Daemon) position(j *job, now time.Time) int {
	n := 0
	for _, t := range d.gateOrder(policy.Order(d.cfg, d.queued(), d.st.Accounts, d.capacity(), now, nil)) {
		if t.GPU != j.GPU {
			continue
		}
		n++
		if t.ID == j.ID {
			return n
		}
	}
	return n + 1
}
