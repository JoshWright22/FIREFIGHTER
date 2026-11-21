package daemon

// Ticket submission, job IDs, and initial resource estimates.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// RequestID is the job id for an idempotent submit: the same project and request key always give
// the same id, so a submit retried after a timeout or a lost reply finds its job (waiting, running
// or done) instead of queuing a second copy (idea from Gczmy/sched's `submit --request-id`).
func RequestID(project, key string) string {
	h := sha256.Sum256([]byte("firefighter-request\x00" + project + "\x00" + key))
	return hex.EncodeToString(h[:4])
}

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Request is a new ticket. With PID set and Running true it adopts an already-running process.
type Request struct {
	policy.Ticket
	Running bool `json:"running"`
	Adopt   bool `json:"adopt"`           // `firefighter adopt`: a process firefighter didn't start (its tree isn't firefighter's to clean up)
	Force   bool `json:"force,omitempty"` // adopt a process from outside the caller's session (the human in tmux/screen)
}

func (d *Daemon) create(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	// Who is really asking: the process on the other end of this connection, not what it claims,
	// and the agent session it belongs to. Looked up before taking the lock (it lists sockets).
	caller := callerPID(r)
	owner, human := d.sessionOf(caller)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	t := req.Ticket
	if t.ID == "" {
		t.ID = NewID()
	}
	if !validID(t.ID) {
		fail(w, 400, "id must be 8 lowercase hex characters")
		return
	}
	if t.Minutes < 0 || math.IsNaN(t.Minutes) {
		t.Minutes = 0
	}
	if lim := d.cfg.Ladder.MaxLeaseMinutes; lim > 0 && t.Minutes > lim {
		// A huge declared lease would dodge the overrun rule and make everyone else's quote say
		// "starts in days".
		t.Minutes = lim
	}
	if t.PID < 0 || t.PID > math.MaxInt32 || t.ClientPID < 0 || t.ClientPID > math.MaxInt32 {
		// Larger values wrap when cast for the OS call, after the checks below compared the full
		// value: 2^32+<guard pid> passed "isn't firefighter itself" and adopted the guard (review 11/20).
		fail(w, 400, "pid out of range")
		return
	}
	if req.Running && (t.PID <= 1 || !sysinfo.Alive(t.PID)) {
		fail(w, 400, fmt.Sprintf("pid %d isn't a running process", t.PID))
		return
	}
	if caller == 0 || (req.Running && caller < 0) {
		// Registering a running process needs to know who asks: never act on a claimed pid.
		fail(w, http.StatusForbidden, "couldn't tell which process is asking; run it from a normal terminal")
		return
	}
	reg := caller
	if req.Running && !req.Adopt && (reg <= 1 || !sysinfo.DescendantOf(t.PID, reg)) {
		// Re-registration after a daemon restart comes from the job's own client.
		fail(w, http.StatusForbidden, fmt.Sprintf("pid %d isn't a job this client started", t.PID))
		return
	}
	if req.Running && req.Adopt && !human && req.Force {
		// --force from an agent reaches only other agents' work: a process with no agent CLI or
		// agent host above it (a browser, an IDE window, a game) is the human's (review 11/20: it
		// would be put on idle priority, suspended first and killed by a cancel).
		g := d.cfgShared.Load().Guard
		if sysinfo.AgentAncestorPID(t.PID, g.AgentParents) == 0 && sysinfo.AgentAncestorPID(t.PID, g.AgentHosts) == 0 {
			fail(w, http.StatusForbidden, fmt.Sprintf("pid %d isn't agent work; only the human adopts other processes (from a terminal window)", t.PID))
			return
		}
	}
	if req.Running && req.Adopt && !human && !req.Force && !sysinfo.DescendantOf(t.PID, owner) {
		// An agent may adopt work from its own session; any other process is the human's to adopt.
		fail(w, http.StatusForbidden, fmt.Sprintf("pid %d isn't from this agent session; adopt it from a terminal window, or add --force", t.PID))
		return
	}
	if caller > 1 {
		t.ClientPID = caller // the real caller, never the body's claim
	}
	if req.Running && (t.PID == os.Getpid() || (runtime.GOOS == "windows" && t.PID <= 4)) {
		fail(w, 400, fmt.Sprintf("pid %d is firefighter itself or the system", t.PID))
		return
	}
	if req.Running && t.ClientPID > 1 && (t.PID == t.ClientPID || sysinfo.DescendantOf(t.ClientPID, t.PID)) {
		// Adopting the caller's own shell or agent would put it (and the caller) on idle priority
		// and kill it with the job.
		fail(w, 400, fmt.Sprintf("pid %d runs the caller itself (its shell or agent); adopt the heavy child instead", t.PID))
		return
	}
	if !req.Running {
		t.PID = 0 // only `start` sets the job's pid, after the turn is granted
	}
	clientAt := int64(0)
	if t.ClientPID > 1 {
		clientAt = sysinfo.Created(t.ClientPID)
	}
	if o, ok := d.prevOpen[t.ID]; ok && (o.Client != caller || (o.ClientAt != 0 && o.ClientAt != clientAt)) && o.Client > 1 && sysinfo.Alive(o.Client) {
		// The previous guard had this job running, and its client is still alive and will re-register:
		// a retry that reaches the new guard first must not start a second copy (review round 5).
		fail(w, http.StatusLocked, "job "+t.ID+" is already running under another client: firefighter wait "+t.ID)
		return
	}
	if old, ok := d.jobs[t.ID]; ok && old.State != "done" {
		if !req.Running && old.ClientPID > 1 && t.ClientPID != old.ClientPID && sysinfo.Alive(old.ClientPID) {
			// Another live client holds this id (two submits with one --request-id raced): answering
			// with its job would let both clients run the command (review 11/20). Wait on it instead.
			fail(w, http.StatusLocked, "job "+t.ID+" is already queued by another client: firefighter wait "+t.ID)
			return
		}
		reply(w, d.view(old)) // re-registration after a daemon restart or a retry
		return
	}
	// An id that already ran belongs to its client: only it may run it again (a requeue), so no
	// one else can take over its receipt or its waiters.
	prevClient, prevAt := 0, int64(0)
	if old, ok := d.jobs[t.ID]; ok {
		prevClient, prevAt = old.ClientPID, old.clientAt
	} else if rc, ok := d.st.Receipts[t.ID]; ok {
		prevClient, prevAt = rc.Client, rc.ClientAt
	}
	if prevClient > 1 && (prevClient != t.ClientPID || (prevAt != 0 && prevAt != clientAt)) {
		fail(w, http.StatusConflict, "job id "+t.ID+" belongs to another client; leave the id out to get a new one")
		return
	}
	// Running and not adopted: a re-registration after a daemon restart. Only a job the previous
	// daemon granted to this same client counts; anything else is outside work brought in.
	reclaim := false
	if req.Running && !req.Adopt {
		if o, ok := d.prevOpen[t.ID]; ok && o.Client == caller && (o.ClientAt == 0 || o.ClientAt == clientAt) {
			reclaim = true
			delete(d.prevOpen, t.ID)
		}
	}
	t.Project, t.Name = cleanText(t.Project, 100), cleanText(t.Name, 120)
	if t.Project == "" {
		t.Project = "default"
	}
	if t.Name == "" {
		t.Name = t.Project
	}
	if t.Quick && (t.Minutes <= 0 || t.Minutes > 3) {
		t.Minutes = 3 // a quick look is short by definition (it skips the storage hold); the CLI refuses a longer one
	}
	if caller > 1 {
		t.ClientPID = caller
	}
	open, total := 0, 0
	for _, o := range d.jobs {
		if o.State != "done" && !o.adopted {
			total++
			if o.Project == t.Project {
				open++
			}
		}
	}
	if !req.Running && !human && (open >= maxOpenPerProject || total >= maxOpenTotal) {
		fail(w, http.StatusTooManyRequests, fmt.Sprintf("too many open jobs (%d in %s, %d in all); wait for some to finish", open, t.Project, total))
		return
	}
	if t.Bid == 0 {
		t.Bid = 5
	}
	if t.Evict != "never" && t.Evict != "force" {
		t.Evict = "graceful"
	}
	t.Bid = max(1, min(10, t.Bid))
	delete(d.st.Receipts, t.ID) // a new attempt under the same id (requeue) replaces the old receipt
	t.Created = now
	if t.NetAuto && !d.cfg.Network.AutoDetect {
		t.Net = false // detected, not declared, and detection is off
	}
	if v := strings.ToLower(t.WantVendor); t.GPU && v != "" && v != "any" {
		if v != "nvidia" && v != "amd" && v != "intel" {
			fail(w, 400, "gpu vendor must be nvidia, amd, intel or any")
			return
		}
		have := false
		for _, g := range d.snap.GPUs {
			have = have || g.Vendor == v
		}
		if !have && len(d.snap.GPUs) > 0 {
			fail(w, 400, fmt.Sprintf("this machine has no %s GPU; use --gpu-vendor any to take what's here", v))
			return
		}
	}
	if t.GPU && t.VRAMGB > 0 && len(d.snap.GPUs) == 0 && !d.gpuSeen && d.snap.RAMTotalGB > 0 && t.VRAMGB > d.snap.RAMTotalGB {
		fail(w, 400, fmt.Sprintf("--vram %.1f GB: this machine has no discrete GPU and only %.1f GB of memory", t.VRAMGB, d.snap.RAMTotalGB))
		return
	}
	if t.GPU && t.VRAMGB > 0 && len(d.snap.GPUs) > 0 {
		biggest := 0.0
		for _, g := range d.snap.GPUs {
			if v := d.vendorFor(t.WantVendor); v == "" || g.Vendor == v { // the cards it would really get
				biggest = math.Max(biggest, g.VRAMGB)
			}
		}
		if biggest > 0 && t.VRAMGB > biggest {
			fail(w, 400, fmt.Sprintf("--vram %.1f GB: no GPU here has that much (largest: %.1f GB)", t.VRAMGB, biggest))
			return
		}
	}
	d.fillNeeds(&t)
	a := d.account(t.Project, now)
	t.Band = policy.BandFor(&t, a.Left(d.cfg, d.cfg.Allowance(t.Project)) > 0)
	t.State = "waiting"
	if req.Running {
		t.State, t.Started = "running", now
	}
	j := newJob(&t)
	j.ownerAgent, j.ownerCreated = owner, sysinfo.Created(owner)
	j.clientAt = clientAt
	j.human = human
	if req.Running && !reclaim {
		// Work started outside the queue and brought in: priced and placed like any adopted
		// process (1.5x, spot, preempted first), so start-then-adopt never beats queueing.
		j.adopted = true
		t.Band = policy.BandSpot
	}
	if req.Running && t.Device != nil {
		// Re-registration: put the job back on the card it was placed on.
		for i, g := range d.snap.GPUs {
			if g.Index == *t.Device && g.Vendor == t.Vendor {
				j.device = i
			}
		}
	}
	if req.Running {
		// Re-registration after a daemon restart: undo any suspend the old daemon left behind.
		for p := range sysinfo.Members(t.PID, j.rootCreated, nil) {
			_ = sysinfo.Resume(p)
		}
		_ = sysinfo.Background(t.PID)
		d.lockPrice(j, now)
	}
	d.jobs[t.ID] = j
	logf("%s [%s] %s: %s, band %d, bid %d, %.0f min declared (%.0f predicted), need %.1f GB RAM %.1f GB VRAM",
		t.Name, t.Project, t.ID, map[bool]string{true: "GPU", false: "CPU"}[t.GPU], t.Band, t.Bid, t.Minutes, t.PredMinutes, t.NeedRAM, t.NeedVRAM)
	d.admit(now)
	reply(w, d.view(j))
}

// Queue-flood guards: per project, and in all (so inventing project names doesn't get around it).
const (
	maxOpenPerProject = 20
	maxOpenTotal      = 60
)

func histKey(t *policy.Ticket) string { return t.Project + "|" + t.CmdHash }

// fillNeeds sets admission needs and the predicted runtime from declarations and learned history.
func (d *Daemon) fillNeeds(t *policy.Ticket) {
	h := d.st.History[histKey(t)]
	known := h != nil
	if h == nil {
		h = &History{}
	}
	// A command never run here starts from what its project's other commands need, not from a
	// fixed default (its own declaration still counts, and its own first run replaces this).
	priorRAM, priorVRAM, prior := 0.0, 0.0, false
	if !known && t.CmdHash != "" {
		priorRAM, priorVRAM, prior = d.projectPrior(t.Project, t.GPU)
	}
	t.PredMinutes = policy.Predict(t.Minutes, h.Ratios)
	t.NeedRAM = math.Max(t.RAMGB, h.PeakRAM*d.cfg.GPU.VRAMMargin)
	if h.RAM != nil && h.RAM.N > 0 {
		t.NeedRAM = math.Max(0.1, usage.PeakNeed(h.RAM, t.RAMGB, 0)) // a known command: its history, not the default below
	}
	// What it can be throttled on is predicted from its past runs' means; unknown stays 0 (no claim).
	t.NeedCPU, t.NeedDisk, t.NeedNet, t.PredGPU = usage.RateNeed(h.CPU, 0), 0, usage.RateNeed(h.Net, 0), usage.RateNeed(h.GPU, 0)
	if h.Disk != nil && h.Disk.N >= usage.WarmRuns-1 {
		t.NeedDisk = usage.PeakNeed(h.Disk, 0, 0)
	}
	if prior && t.RAMGB == 0 {
		t.NeedRAM = math.Max(0.1, priorRAM)
	}
	if t.NeedRAM == 0 {
		t.NeedRAM = 0.5
		if t.GPU && (len(d.snap.GPUs) > 0 || d.gpuSeen) && d.cfg.GPU.DefaultRAMGB > t.NeedRAM {
			// Nothing declared, nothing learned: a GPU framework alone takes a few GB of RAM, and a
			// job granted on a 0.5 GB guess is the one stopped minutes later.
			t.NeedRAM = d.cfg.GPU.DefaultRAMGB
		}
	}
	// A container's --memory is what it may take (inside the WSL VM, which takes it from Windows),
	// so a smaller declaration alone can't keep the machine out of paging (2026-11-20: --ram 6,
	// --memory=28g). The reservation is the declared or learned need with half again on top (the
	// whole limit when nothing is known), never above the limit; once the container runs it is held
	// to that reservation (docker update), so it can't grow past what admission counted.
	if dm := dockerMemGB(t.Command); dm > t.NeedRAM {
		want := dm
		if t.RAMGB > 0 || (h.RAM != nil && h.RAM.N > 0) {
			want = math.Min(dm, math.Max(1, t.NeedRAM*1.5))
		}
		t.NeedRAM = math.Max(t.NeedRAM, want)
	}
	t.WantRAM = 0
	if most := d.ramMost(); d.snap.RAMTotalGB > 0 && t.NeedRAM > most {
		// A need the machine can never free would hold the line forever: the job starts when the
		// machine is at its emptiest, and its status line says so.
		t.WantRAM, t.NeedRAM = t.NeedRAM, most
	}
	if t.GPU {
		t.NeedVRAM = math.Max(t.VRAMGB, h.PeakVRAM*d.cfg.GPU.VRAMMargin)
		if h.VRAM != nil && h.VRAM.N > 0 {
			t.NeedVRAM = usage.PeakNeed(h.VRAM, t.VRAMGB, 0)
		}
		if prior && t.VRAMGB == 0 && priorVRAM > 0 {
			t.NeedVRAM = priorVRAM
		}
		if t.NeedVRAM == 0 {
			t.NeedVRAM = d.cfg.GPU.DefaultVRAMGB
		}
		if len(d.snap.GPUs) == 0 && !d.gpuSeen {
			// No discrete GPU (Apple silicon, iGPU): VRAM is system RAM, so admit on RAM. (A
			// machine that has reported a GPU and missed one reading isn't this case.)
			t.NeedRAM += t.NeedVRAM
			t.NeedVRAM = 0
			if most := d.ramMost(); d.snap.RAMTotalGB > 0 && t.NeedRAM > most {
				t.WantRAM, t.NeedRAM = t.NeedRAM, most
			}
		}
		// No cap here: which card the job gets is decided at admission, where needOn clamps a
		// need to what that card ever frees.
	}
}
