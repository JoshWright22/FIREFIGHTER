package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	psnet "github.com/shirou/gopsutil/v4/net"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Endpoint is how clients find the daemon: written to <home>/daemon.json.
type Endpoint struct {
	PID     int    `json:"pid"`
	Created int64  `json:"created"` // the daemon's start time: a reused pid is never mistaken for it
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Version string `json:"version"`
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func EndpointPath() string { return filepath.Join(config.Home(), "daemon.json") }

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Serve listens on localhost, publishes the endpoint, and runs the control loop.
func (d *Daemon) Serve() error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	d.Token, d.Port = hex.EncodeToString(tok), ln.Addr().(*net.TCPAddr).Port
	logf("daemon up on 127.0.0.1:%d (pid %d)", d.Port, os.Getpid())
	d.mu.Lock()
	d.step(d.reader.Read(), nil, nil, time.Now(), 0) // measure before the first ticket arrives
	d.mu.Unlock()
	go d.Run()
	b, _ := json.Marshal(Endpoint{PID: os.Getpid(), Created: sysinfo.Created(os.Getpid()), Port: d.Port, Token: d.Token, Version: d.Version})
	if err := writeAtomic(EndpointPath(), b, 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-d.stop
		_ = srv.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"ok": true, "pid": os.Getpid(), "version": d.Version})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true})
		logf("shutdown requested")
		if !d.stopping.CompareAndSwap(false, true) {
			return // already shutting down
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			d.mu.Lock()
			d.release()
			_ = d.st.write()
			d.mu.Unlock()
			close(d.stop)
		}()
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		reply(w, d.status())
	})
	mux.HandleFunc("POST /v1/tickets", d.create)
	mux.HandleFunc("GET /v1/quote", d.quoteHandler)
	mux.HandleFunc("GET /v1/tickets/{id}", d.get)
	mux.HandleFunc("POST /v1/tickets/{id}/{verb}", d.act)
	mux.HandleFunc("POST /v1/plans", d.plan)
	mux.HandleFunc("DELETE /v1/plans/{id}", d.unplan)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only local clients by name (defense in depth against DNS rebinding; the token already
		// can't be sent by a page without a CORS preflight, which fails).
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			h = strings.Trim(r.Host, "[]") // no port given
		}
		if h != "127.0.0.1" && h != "localhost" && h != "::1" {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Firefighter-Token") != d.Token {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { http.Error(w, msg, code) }

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
	if req.Running && (t.PID <= 1 || !sysinfo.Alive(t.PID)) {
		fail(w, 400, fmt.Sprintf("pid %d isn't a running process", t.PID))
		return
	}
	if caller == 0 || (req.Running && caller < 0 && req.Adopt) {
		fail(w, http.StatusForbidden, "couldn't tell which process is asking; adopt from a normal terminal")
		return
	}
	reg := caller
	if reg < 0 { // no connection table here: trust the client's claim for its own job only
		reg = t.ClientPID
	}
	if req.Running && !req.Adopt && (reg <= 1 || !sysinfo.DescendantOf(t.PID, reg)) {
		// Re-registration after a daemon restart comes from the job's own client.
		fail(w, http.StatusForbidden, fmt.Sprintf("pid %d isn't a job this client started", t.PID))
		return
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
	if old, ok := d.jobs[t.ID]; ok && old.State != "done" {
		reply(w, d.view(old)) // re-registration after a daemon restart or a retry
		return
	}
	t.Project, t.Name = cleanText(t.Project, 100), cleanText(t.Name, 120)
	if t.Project == "" {
		t.Project = "default"
	}
	if t.Name == "" {
		t.Name = t.Project
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
	if !req.Running && (open >= maxOpenPerProject || total >= maxOpenTotal) {
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
	d.fillNeeds(&t)
	a := d.account(t.Project, now)
	t.Band = policy.BandFor(&t, a.Left(d.cfg, d.cfg.Allowance(t.Project)) > 0)
	t.State = "waiting"
	if req.Running {
		t.State, t.Started = "running", now
	}
	j := newJob(&t)
	j.ownerAgent, j.ownerCreated = owner, sysinfo.Created(owner)
	if req.Running && req.Adopt {
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
	if h == nil {
		h = &History{}
	}
	t.PredMinutes = policy.Predict(t.Minutes, h.Ratios)
	t.NeedRAM = math.Max(t.RAMGB, h.PeakRAM*d.cfg.GPU.VRAMMargin)
	if t.NeedRAM == 0 {
		t.NeedRAM = 0.5
	}
	if most := d.snap.RAMTotalGB * (1 - d.cfg.Reserve.Away.RAMPct/100) * 0.9; most > 0 && t.NeedRAM > most {
		t.NeedRAM = most // a need the machine can never free would hold the line forever
	}
	if t.GPU {
		t.NeedVRAM = math.Max(t.VRAMGB, h.PeakVRAM*d.cfg.GPU.VRAMMargin)
		if t.NeedVRAM == 0 {
			t.NeedVRAM = d.cfg.GPU.DefaultVRAMGB
		}
		if len(d.snap.GPUs) == 0 {
			// No discrete GPU (Apple silicon, iGPU): VRAM is system RAM, so admit on RAM.
			t.NeedRAM += t.NeedVRAM
			t.NeedVRAM = 0
		}
		// No cap here: which card the job gets is decided at admission, where needOn clamps a
		// learned need to what that card ever frees (a declared --vram is never shrunk).
	}
}

// get returns a ticket. ?wait=N long-polls up to N seconds for a change of state from ?state=.
func (d *Daemon) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait, _ := strconv.ParseFloat(r.URL.Query().Get("wait"), 64)
	from := r.URL.Query().Get("state")
	deadline := time.Now().Add(time.Duration(math.Min(wait, 60) * float64(time.Second)))
	for {
		d.mu.Lock()
		j, ok := d.jobs[id]
		if !ok {
			rec := d.st.Receipts[id]
			d.mu.Unlock()
			if rec != nil && from == "" {
				reply(w, View{ID: id, State: "done", Receipt: rec})
				return
			}
			fail(w, 404, "no such job "+id)
			return
		}
		v := d.view(j)
		ch := d.notify
		d.mu.Unlock()
		if from == "" || v.State != from || !time.Now().Before(deadline) {
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

type Act struct {
	PID       int     `json:"pid"`
	Exit      int     `json:"exit"`
	Minutes   float64 `json:"minutes"`
	Log       string  `json:"log"`
	Container string  `json:"container"`
	// Project is the caller's project: cancel, bid and extend on another project's job need Force.
	// A guard against agents touching each other's jobs by accident, not a security boundary.
	Project string `json:"project,omitempty"`
	Force   bool   `json:"force,omitempty"`
	Engine  string `json:"engine,omitempty"` // container engine: docker | podman
}

// validID: ticket ids end up in file names, so only what NewID makes is accepted.
func validID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (d *Daemon) act(w http.ResponseWriter, r *http.Request) {
	var a Act
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			fail(w, 400, "bad body: "+err.Error())
			return
		}
	}
	caller := callerPID(r)
	callerAgent, callerHuman := d.sessionOf(caller)
	callerAgentAt := sysinfo.Created(callerAgent)
	d.mu.Lock()
	defer d.mu.Unlock()
	id, verb := r.PathValue("id"), r.PathValue("verb")
	j, ok := d.jobs[id]
	if !ok {
		fail(w, 404, "no such job "+id)
		return
	}
	now := time.Now()
	// Who may change a job: the human (no agent above the caller: a terminal, the VS Code button),
	// the agent session that submitted it, or an agent in the same project. Other agents need
	// --force. A guard against agents touching each other's work by accident, not a sandbox.
	if verb == "cancel" || verb == "bid" || verb == "extend" || verb == "done" {
		sameSession := callerAgent != 0 && (callerAgent == j.ownerAgent && callerAgentAt == j.ownerCreated ||
			caller == j.ClientPID) // the job's own client always may (an unknown agent's own job)
		if !callerHuman && !sameSession && cleanText(a.Project, 100) != j.Project && !a.Force {
			fail(w, http.StatusForbidden, fmt.Sprintf("job %s belongs to another agent session (project %s); add --force if you mean it", id, j.Project))
			return
		}
	}
	if caller == 0 {
		fail(w, http.StatusForbidden, "couldn't tell which process is asking (half-closed or foreign connection)")
		return
	}
	switch verb {
	case "start":
		if j.State != "granted" {
			fail(w, http.StatusConflict, "job "+id+" is "+j.State+", not granted")
			return
		}
		if a.PID <= 1 || !sysinfo.Alive(a.PID) {
			fail(w, 400, fmt.Sprintf("pid %d isn't a running process", a.PID))
			return
		}
		starter := caller
		if starter < 0 { // the connection table isn't available on this system
			starter = j.ClientPID
		}
		if j.ClientPID > 1 && starter != j.ClientPID {
			fail(w, http.StatusForbidden, "only the client that queued this job can start it")
			return
		}
		if starter <= 1 || !sysinfo.DescendantOf(a.PID, starter) {
			// The job is a process its client started. Anything else (a game, a browser, another
			// agent) must not be made idle-priority and killed with this job.
			fail(w, http.StatusForbidden, fmt.Sprintf("pid %d wasn't started by this job's client", a.PID))
			return
		}
		j.State, j.Started, j.PID = "running", now, a.PID
		j.rootCreated = sysinfo.Created(a.PID)
		j.pids = map[int]int64{a.PID: j.rootCreated}
		if j.price == 0 {
			d.lockPrice(j, now)
		}
		if err := sysinfo.Background(a.PID); err != nil {
			logf("%s: background priority: %v", j.Name, err)
		}
		logf("%s: started pid %d after %.1f min", j.Name, a.PID, now.Sub(j.Created).Minutes())
	case "container":
		if a.Container == "" || (j.ClientPID > 1 && caller != j.ClientPID && caller >= 0) {
			fail(w, http.StatusForbidden, "only the job's own client can name its container")
			return
		}
		if a.Container != j.container {
			// Verified before the lock below would matter: only a container labeled for this job.
			d.mu.Unlock()
			ok := containerFor(a.Engine, a.Container, j.ID)
			d.mu.Lock()
			if !ok {
				fail(w, http.StatusForbidden, "container "+a.Container+" isn't labeled firefighter.job="+j.ID)
				return
			}
		}
		j.container, j.engine = a.Container, a.Engine
	case "bid": // re-bid while waiting, after reading the market
		if a.Minutes < 1 || a.Minutes > 10 {
			fail(w, 400, "bid must be 1-10")
			return
		}
		if j.State == "waiting" {
			j.Bid = max(1, min(10, int(a.Minutes)))
			logf("%s: re-bid %d", j.Name, j.Bid)
		}
	case "extend":
		if a.Minutes <= 0 || a.Minutes > 24*60 {
			fail(w, 400, "extend by 1 to 1440 minutes")
			return
		}
		if j.backfilled {
			fail(w, http.StatusConflict, "this job started early (backfill) on a short lease; it can't be extended. Requeue the rest with an honest --minutes")
			return
		}
		if lim := d.cfg.Ladder.MaxLeaseMinutes; lim > 0 && j.Minutes+j.extended+a.Minutes > lim {
			fail(w, http.StatusConflict, fmt.Sprintf("a lease can't pass %.0f minutes; requeue the rest", lim))
			return
		}
		// An extension pushes the lease out at today's price (no keeping a 0.5x idle-hour price for
		// a day), and an overrun already charged at 2x stays that way.
		j.extended += a.Minutes
		d.lockPrice(j, now)
		logf("%s: lease extended by %.0f min", j.Name, a.Minutes)
	case "done":
		outcome := "ok"
		if a.Exit != 0 {
			outcome = "failed"
		}
		if !j.checkpoint.IsZero() || a.Exit == 75 { // 75 = checkpointed, requeue me (EX_TEMPFAIL)
			outcome = "evicted"
		}
		if !j.killedAt.IsZero() && j.Evict == "force" {
			outcome = "killed"
		}
		d.finish(id, a.Exit, outcome, a.Log)
	case "cancel":
		if j.State == "running" {
			j.reason = "cancelled"
			d.killJob(j, now)
		}
		d.finish(id, -1, "cancelled", "")
	default:
		fail(w, 404, "unknown action "+verb)
		return
	}
	d.broadcast()
	reply(w, d.view(j))
}

// finish closes a ticket: learns runtime and peaks, writes the receipt.
func (d *Daemon) finish(id string, exit int, outcome, log string) {
	j := d.jobs[id]
	if j == nil || j.State == "done" {
		return
	}
	now := time.Now()
	ran := 0.0
	if !j.Started.IsZero() {
		ran = now.Sub(j.Started).Minutes()
	}
	if j.State == "running" && outcome != "lost" {
		h := d.st.History[histKey(j.Ticket)]
		if h == nil {
			h = &History{}
			d.st.History[histKey(j.Ticket)] = h
		}
		if j.Minutes > 0 && outcome == "ok" && ran >= 0.5 { // sub-30 s runs say nothing about runtime
			h.Ratios = append(h.Ratios, math.Max(0.05, ran/j.Minutes))
			if len(h.Ratios) > 5 {
				h.Ratios = h.Ratios[len(h.Ratios)-5:]
			}
		}
		h.PeakVRAM = math.Max(h.PeakVRAM*0.9, j.peakVRAM) // slowly forgets old peaks
		h.PeakRAM = math.Max(h.PeakRAM*0.9, j.peakRAM)
		h.Runs++
		h.Last = now
	}
	if !j.suspended.IsZero() {
		d.resume(j)
	}
	// Workers that outlive their job (data loaders holding VRAM) would make the next GPU job run
	// out of memory: a finished job's leftovers go with it. Adopted processes are left alone, and so
	// are shared build servers (guard.keep_alive) the human's own builds reuse.
	if !j.adopted && j.State == "running" {
		for p, created := range j.pids {
			if p != j.PID && sysinfo.Same(p, created) && !keepAlive(d.cfg.Guard.KeepAlive, sysinfo.Name(p)) {
				_ = sysinfo.Kill(p)
			}
		}
		// A container started by `docker run` outlives its CLI when the client is interrupted.
		if j.container != "" && outcome != "ok" && outcome != "failed" {
			d.docker(j.engine, "kill", j.container)
		}
	}
	for _, f := range []string{PauseFile(id), PausedFile(id), CheckpointFile(id)} {
		_ = os.Remove(f)
	}
	waited := now.Sub(j.Created).Minutes() - ran
	rec := &Receipt{ID: id, Project: j.Project, Name: j.Name, Exit: exit, Outcome: outcome, Minutes: round1(ran),
		Declared: j.Minutes, Waited: round1(waited), Charged: round1(j.charged), PeakVRAM: round1(j.peakVRAM),
		PeakRAM: round1(j.peakRAM), Log: log, Ended: now, Requeue: outcome == "evicted"}
	j.receipt = rec
	j.State = "done"
	d.st.Receipts[id] = rec
	logf("%s: %s (exit %d) after %.1f min, waited %.1f, charged %.1f", j.Name, outcome, exit, ran, waited, j.charged)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func keepAlive(names []string, name string) bool {
	n := strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".exe"))
	for _, k := range names {
		if k != "" && strings.Contains(n, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// clock formats a time the way people in the US read it: "3:04 PM".
func clock(t time.Time) string { return t.Format("3:04 PM") }

// ---- views --------------------------------------------------------------------------------------

type View struct {
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Project  string        `json:"project,omitempty"`
	State    string        `json:"state"` // waiting | granted | running | done
	Band     int           `json:"band,omitempty"`
	Position int           `json:"position,omitempty"` // in its line (GPU or CPU), 1 = next
	ETA      string        `json:"eta,omitempty"`      // when it should start (waiting) or end (running)
	Why      string        `json:"why,omitempty"`      // what it's waiting for, or why it's paused
	Paused   bool          `json:"paused,omitempty"`
	Checkpt  bool          `json:"checkpoint,omitempty"`
	StartsIn float64       `json:"starts_in_min,omitempty"`
	Tier     string        `json:"tier,omitempty"` // spot | normal | urgent
	Budget   float64       `json:"budget_left"`    // project's machine-minutes left
	Pred     float64       `json:"predicted_minutes,omitempty"`
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
	a := d.peek(j.Project, now)
	v := View{ID: j.ID, Name: j.Name, Project: j.Project, State: j.State, Band: j.Band, Tier: tierOf(j.Ticket),
		Budget: round1(math.Max(-1e9, math.Min(1e9, a.Left(d.cfg, d.cfg.Allowance(j.Project))))), Pred: round1(j.PredMinutes),
		Receipt: j.receipt, Paused: !j.pausedAt.IsZero(), Checkpt: !j.checkpoint.IsZero(), Why: j.reason,
		PauseF: PauseFile(j.ID), PausedF: PausedFile(j.ID), CheckF: CheckpointFile(j.ID)}
	if j.device >= 0 && j.device < len(d.snap.GPUs) {
		g := d.snap.GPUs[j.device]
		dev := g.Index
		v.Device, v.Vendor = &dev, g.Vendor
	}
	switch j.State {
	case "waiting":
		// One source for every estimate: the market's schedule simulation, which counts the human
		// holding the GPU and GPU work outside firefighter.
		q := d.quote(*j.Ticket, now)
		own, _ := q.Tier(tierOf(j.Ticket))
		v.Position, v.Why = own.Position, d.blocker(j)
		v.ETA = clock(now.Add(time.Duration(own.StartsIn * float64(time.Minute))))
		v.StartsIn = own.StartsIn
		v.Quote, v.Market = &q, q.Line()
	case "running":
		left := policy.Remaining(j.PredMinutes+j.extended, j.leaseUsed(now).Minutes())
		v.ETA = clock(now.Add(time.Duration(left * float64(time.Minute))))
	}
	return v
}

func (d *Daemon) blocker(j *job) string {
	ram, _ := d.headroom(time.Now())
	switch {
	case j.GPU && d.gpuHeld():
		return "the human needs the GPU (" + d.why() + ")"
	case j.Band == policy.BandSpot:
		return "spot band (over budget or --spot): runs on leftovers only"
	case j.GPU && len(d.snap.GPUs) > 0 && func() bool { c, _ := d.cardFor(j.WantVendor); return c < 0 }():
		return "waiting for a " + j.WantVendor + " GPU (none is reporting right now)"
	case j.GPU && len(d.snap.GPUs) > 0 && d.jobNeed(j) > d.cardLeft(j.WantVendor):
		return fmt.Sprintf("needs %.1f GB VRAM, %.1f GB free after the human's reserve", d.jobNeed(j), math.Max(0, d.cardLeft(j.WantVendor)))
	case j.NeedRAM > ram:
		return fmt.Sprintf("needs %.1f GB RAM, %.1f GB free after the human's reserve", j.NeedRAM, math.Max(0, ram))
	case j.GPU && len(d.busyOutside()) > 0:
		return "GPU busy outside firefighter: " + strings.Join(d.busyOutside(), ", ")
	case j.GPU:
		return "GPU turn queue"
	case j.Band == policy.BandSpot:
		return "spot: leftovers only"
	}
	return "starting"
}

func (d *Daemon) busyOutside() []string {
	var out []string
	for n, u := range d.foreign {
		if u >= d.cfg.Reserve.ForeignGPUPct {
			out = append(out, fmt.Sprintf("%s %.0f%%", n, u))
		}
	}
	return out
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
	Market    map[string]policy.Quote `json:"market"` // posted prices and odds for a typical 30-minute job
}

func (d *Daemon) status() Status {
	now := time.Now()
	vt, vu := d.snap.VRAMTotal()
	s := Status{Time: now, User: d.user, Plans: d.st.Plans, Foreign: d.foreign, Docker: d.others, GPUs: d.snap.GPUs,
		ConfigErr: d.cfgErr, Budgets: map[string]float64{},
		Reserve: map[string]float64{"cpus": d.res.CPUs, "ram_pct": d.res.RAMPct, "vram_gb": d.res.VRAMGB},
		Machine: map[string]float64{"cpus": float64(d.snap.CPUs), "cpu_busy": round1(d.snap.CPUBusy), "user_cpus": round1(d.busyUser),
			"ram_total_gb": round1(d.snap.RAMTotalGB), "ram_avail_gb": round1(d.snap.RAMAvailGB),
			"vram_total_gb": round1(vt), "vram_used_gb": round1(vu), "gpu_util": d.snap.GPUUtil(), "idle_s": math.Round(d.snap.Idle)},
		Policy: map[string]string{"preset": d.cfg.Preset, "order": d.cfg.Order.Policy, "pricing": d.cfg.Pricing.Policy, "budget": d.cfg.Budget.Policy}}
	if d.user == "game" {
		s.UserWhy = d.gameWhy
	}
	s.Market = map[string]policy.Quote{}
	for _, gpu := range []bool{true, false} {
		p := policy.Ticket{Project: "default", GPU: gpu, Minutes: 30, Bid: policy.BidNormal}
		d.fillNeeds(&p)
		s.Market[map[bool]string{true: "gpu", false: "cpu"}[gpu]] = d.quote(p, now)
	}
	for _, j := range d.running() {
		s.Running = append(s.Running, d.view(j))
	}
	for _, t := range d.waiting() {
		s.Waiting = append(s.Waiting, d.view(d.jobs[t.ID]))
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

func (d *Daemon) plan(w http.ResponseWriter, r *http.Request) {
	var p Plan
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if p.Minutes <= 0 || p.Minutes > 7*24*60 || p.GPUPct < 0 || p.GPUPct > 100 || p.VRAMGB < 0 || p.VRAMGB > 4096 ||
		p.RAMGB < 0 || p.RAMGB > 4096 || len(p.What) > 300 || len(p.When) > 40 || len(p.Project) > 200 {
		fail(w, 400, "plan: minutes 1-10080, gpu 0-100, sizes 0-4096 GB, short text")
		return
	}
	p.Project, p.What, p.When = cleanText(p.Project, 100), cleanText(p.What, 300), cleanText(p.When, 40)
	planOwner, planHuman := d.sessionOf(callerPID(r))
	if planHuman {
		planOwner = -1 // the human's: only the human removes it
	}
	planOwnerAt := sysinfo.Created(planOwner)
	d.mu.Lock()
	defer d.mu.Unlock()
	mine := 0
	for _, o := range d.st.Plans {
		if o.Project == p.Project {
			mine++
		}
	}
	if mine >= 10 || len(d.st.Plans) >= 100 {
		fail(w, http.StatusTooManyRequests, "too many plans; remove some with `firefighter plan --done`")
		return
	}
	p.ID, p.Added = NewID(), time.Now()
	p.Owner, p.OwnerAt = planOwner, planOwnerAt
	d.st.Plans = append(d.st.Plans, &p)
	reply(w, p)
}

func (d *Daemon) unplan(w http.ResponseWriter, r *http.Request) {
	caller := callerPID(r)
	agent, human := d.sessionOf(caller)
	agentAt := sysinfo.Created(agent)
	d.mu.Lock()
	defer d.mu.Unlock()
	id := r.PathValue("id")
	var keep []*Plan
	for _, p := range d.st.Plans {
		// The human removes any plan; an agent removes its own session's (or, after a restart
		// forgot who added it, its project's).
		// Like jobs: the human, the session that announced it, or its project (by project name).
		// A plan id after a restart forgot who added it: whoever names the id.
		if p.Owner == -1 && !human {
			keep = append(keep, p) // the human's plan
			continue
		}
		mine := human || (p.Owner > 0 && p.Owner == agent && p.OwnerAt == agentAt) || p.Project == id ||
			(p.ID == id && p.Owner == 0)
		if (p.ID == id || p.Project == id) && mine {
			continue
		}
		keep = append(keep, p)
	}
	d.st.Plans = keep
	reply(w, map[string]int{"plans": len(keep)})
}

// writeStatus mirrors the status to STATUS.md for agents that read files instead of calling tools.
func (d *Daemon) writeStatus() {
	s := d.status()
	var b strings.Builder
	fmt.Fprintf(&b, "# firefighter status, %s\n\nRewritten every 15 s. Older than a minute means the guard is down: run `firefighter daemon`.\n\n", s.Time.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Human: **%s** %s. Reserve kept for them: %.0f CPUs, %.0f%% RAM, %.1f GB VRAM.\n\n", s.User, s.UserWhy, s.Reserve["cpus"], s.Reserve["ram_pct"], s.Reserve["vram_gb"])
	fmt.Fprintf(&b, "| Resource | Use |\n|---|---|\n| CPU | %.1f of %.0f busy (%.1f by the human and other programs) |\n| RAM | %.1f of %.1f GB free |\n| GPU | %.0f%% |\n| VRAM | %.1f of %.1f GB used |\n\n",
		s.Machine["cpu_busy"], s.Machine["cpus"], s.Machine["user_cpus"], s.Machine["ram_avail_gb"], s.Machine["ram_total_gb"], s.Machine["gpu_util"], s.Machine["vram_used_gb"], s.Machine["vram_total_gb"])
	b.WriteString("## Running (busy until)\n\n| Job | Project | Until | Note |\n|---|---|---|---|\n")
	for _, v := range s.Running {
		fmt.Fprintf(&b, "| %s | %s | ~%s | %s |\n", v.Name, v.Project, v.ETA, v.Why)
	}
	b.WriteString("\n## Waiting\n\n| # | Job | Project | Starts ~ | Waiting for |\n|---|---|---|---|---|\n")
	for _, v := range s.Waiting {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", v.Position, v.Name, v.Project, v.ETA, v.Why)
	}
	b.WriteString("\n## Planned\n\n")
	for _, p := range s.Plans {
		fmt.Fprintf(&b, "- %s: %s, %s, ~%.0f min, GPU %.0f%%, %.1f GB VRAM, %.1f GB RAM\n", p.Project, p.What, p.When, p.Minutes, p.GPUPct, p.VRAMGB, p.RAMGB)
	}
	_ = os.WriteFile(filepath.Join(config.Home(), "STATUS.md"), []byte(b.String()), 0o600)
}

// containerFor says whether a container carries the label firefighter put on this job's docker run.
func containerFor(engine, name, id string) bool {
	if strings.HasPrefix(name, "-") {
		return false
	}
	if engine != "podman" {
		engine = "docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, engine, "inspect", "-f", `{{index .Config.Labels "firefighter.job"}}`, name)
	sysinfo.PrepareBackground(c)
	out, err := c.Output()
	return err == nil && strings.TrimSpace(string(out)) == id
}

// cleanText drops control characters (newlines, terminal escapes) and trims to max runes: text one
// agent sends is shown to other agents and printed in the human's terminal, so it must not forge
// lines (a fake "do:") or carry escape sequences.
func cleanText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if sysinfo.Printable(string(r)) == "" {
			r = ' '
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// callerPID finds the local process on the other end of a request's TCP connection (0 if unknown).
// It matches the whole established connection (the client's address and port, and ours), so a
// process can't pose as another by binding a port that one listens on. -1: the lookup itself isn't
// available here (callers fall back); 0: no such connection (the request is refused).
func callerPID(r *http.Request) int {
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() { // all of 127/8 and ::1
		return 0
	}
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return -1
	}
	lhost, lport, err := net.SplitHostPort(local.String())
	if err != nil {
		return -1
	}
	p, _ := strconv.Atoi(port)
	lp, _ := strconv.Atoi(lport)
	conns, err := psnet.Connections("tcp")
	if err != nil || len(conns) == 0 {
		return -1
	}
	for _, c := range conns {
		if c.Pid > 0 && c.Pid != int32(os.Getpid()) && strings.EqualFold(c.Status, "ESTABLISHED") &&
			int(c.Laddr.Port) == p && sameIP(c.Laddr.IP, host) && int(c.Raddr.Port) == lp && sameIP(c.Raddr.IP, lhost) {
			return int(c.Pid)
		}
	}
	return 0
}

func sameIP(a, b string) bool {
	x, y := net.ParseIP(a), net.ParseIP(b)
	return x != nil && y != nil && x.Equal(y)
}

// sessionOf names the agent session a caller belongs to: the nearest agent CLI above it, else (when
// the caller isn't a provable human session) the nearest agent host such as node or an IDE (one
// session per host process), else the caller itself. human: a terminal or the desktop started it,
// with no agent in between.
func (d *Daemon) sessionOf(caller int) (session int, human bool) {
	if caller <= 1 {
		return 0, false
	}
	g := d.cfg.Guard
	if a := sysinfo.AgentAncestorPID(caller, g.AgentParents); a != 0 {
		return a, false
	}
	if sysinfo.HumanSession(caller, g.HumanHosts, g.AgentParents, g.AgentHosts) {
		return 0, true
	}
	if h := sysinfo.AgentAncestorPID(caller, g.AgentHosts); h != 0 {
		return h, false
	}
	return caller, false
}

// jobNeed is a job's VRAM need on the card it would get (see needOn).
func (d *Daemon) jobNeed(j *job) float64 {
	card, _ := d.cardFor(j.WantVendor)
	return d.needOn(j.Ticket, card)
}
