package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Endpoint is how clients find the daemon: written to <home>/daemon.json.
type Endpoint struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func EndpointPath() string { return filepath.Join(config.Home(), "daemon.json") }

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Serve listens on localhost, publishes the endpoint, and runs the control loop.
func (d *Daemon) Serve() error {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
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
	d.step(d.reader.Read(), time.Now(), 0) // measure before the first ticket arrives
	d.mu.Unlock()
	go d.Run()
	b, _ := json.Marshal(Endpoint{PID: os.Getpid(), Port: d.Port, Token: d.Token})
	if err := os.WriteFile(EndpointPath(), b, 0o600); err != nil {
		return err
	}
	return http.Serve(ln, d.routes())
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]any{"ok": true, "pid": os.Getpid()}) })
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
		if r.Header.Get("X-Firefighter-Token") != d.Token {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
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
}

func (d *Daemon) create(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	t := req.Ticket
	if t.ID == "" {
		t.ID = NewID()
	}
	if old, ok := d.jobs[t.ID]; ok && old.State != "done" {
		reply(w, d.view(old)) // re-registration after a daemon restart or a retry
		return
	}
	if t.Project == "" {
		t.Project = "default"
	}
	if t.Name == "" {
		t.Name = t.Project
	}
	open := 0
	for _, o := range d.jobs {
		if o.Project == t.Project && o.State != "done" {
			open++
		}
	}
	if open >= maxOpenPerProject && !req.Running {
		fail(w, http.StatusTooManyRequests, fmt.Sprintf("project %s already has %d jobs queued or running; wait for some to finish", t.Project, open))
		return
	}
	if t.Bid == 0 {
		t.Bid = 5
	}
	if t.Evict == "" {
		t.Evict = "graceful"
	}
	t.Created = now
	d.fillNeeds(&t)
	a := d.account(t.Project, now)
	t.Band = policy.BandFor(&t, a.Left(d.cfg, d.cfg.Allowance(t.Project)) > 0)
	t.State = "waiting"
	if req.Running {
		t.State, t.Started = "running", now
	}
	j := &job{Ticket: &t, pids: map[int]bool{}, applied: map[int]string{}}
	if t.PID > 0 {
		j.pids[t.PID] = true
		_ = sysinfo.Background(t.PID)
	}
	if req.Running {
		d.lockPrice(j, now)
	}
	d.jobs[t.ID] = j
	logf("%s [%s] %s: %s, band %d, bid %d, %.0f min declared (%.0f predicted), need %.1f GB RAM %.1f GB VRAM",
		t.Name, t.Project, t.ID, map[bool]string{true: "GPU", false: "CPU"}[t.GPU], t.Band, t.Bid, t.Minutes, t.PredMinutes, t.NeedRAM, t.NeedVRAM)
	d.admit(now)
	reply(w, d.view(j))
}

const maxOpenPerProject = 20 // queue-flood guard

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
	if t.GPU {
		t.NeedVRAM = math.Max(t.VRAMGB, h.PeakVRAM*d.cfg.GPU.VRAMMargin)
		if t.NeedVRAM == 0 {
			t.NeedVRAM = d.cfg.GPU.DefaultVRAMGB
		}
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
		if from == "" || v.State != from || !time.Now().Before(deadline) {
			reply(w, v)
			return
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			return
		}
	}
}

type Act struct {
	PID       int     `json:"pid"`
	Exit      int     `json:"exit"`
	Minutes   float64 `json:"minutes"`
	Log       string  `json:"log"`
	Container string  `json:"container"`
}

func (d *Daemon) act(w http.ResponseWriter, r *http.Request) {
	var a Act
	_ = json.NewDecoder(r.Body).Decode(&a)
	d.mu.Lock()
	defer d.mu.Unlock()
	id, verb := r.PathValue("id"), r.PathValue("verb")
	j, ok := d.jobs[id]
	if !ok {
		fail(w, 404, "no such job "+id)
		return
	}
	now := time.Now()
	switch verb {
	case "start":
		j.State, j.Started, j.PID = "running", now, a.PID
		j.pids[a.PID] = true
		if err := sysinfo.Background(a.PID); err != nil {
			logf("%s: background priority: %v", j.Name, err)
		}
		logf("%s: started pid %d after %.1f min", j.Name, a.PID, now.Sub(j.Created).Minutes())
	case "container":
		j.container = a.Container
	case "bid": // re-bid while waiting, after reading the market
		if j.State == "waiting" {
			j.Bid = max(1, min(10, int(a.Minutes)))
			logf("%s: re-bid %d", j.Name, j.Bid)
		}
	case "extend":
		j.extended += a.Minutes
		if j.overrun {
			j.overrun = false
			a := d.account(j.Project, now)
			j.Band = policy.BandFor(j.Ticket, a.Left(d.cfg, d.cfg.Allowance(j.Project)) > 0)
		}
		logf("%s: lease extended by %.0f min", j.Name, a.Minutes)
	case "done":
		outcome := "ok"
		if a.Exit != 0 {
			outcome = "failed"
		}
		if !j.checkpoint.IsZero() {
			outcome = "evicted"
		}
		d.finish(id, a.Exit, outcome, a.Log)
	case "cancel":
		if j.State == "running" {
			j.reason = "cancelled"
			d.killJob(j)
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
		if j.Minutes > 0 && outcome == "ok" {
			h.Ratios = append(h.Ratios, math.Max(0.05, ran/j.Minutes))
			if len(h.Ratios) > 5 {
				h.Ratios = h.Ratios[len(h.Ratios)-5:]
			}
		}
		h.PeakVRAM = math.Max(h.PeakVRAM*0.9, j.peakVRAM) // slowly forgets old peaks
		h.PeakRAM = math.Max(h.PeakRAM*0.9, j.peakRAM)
		h.Runs++
	}
	for p := range j.pids {
		if !j.suspended.IsZero() {
			_ = sysinfo.Resume(p)
		}
	}
	for _, f := range []string{PauseFile(id), PausedFile(id), CheckpointFile(id)} {
		_ = os.Remove(f)
	}
	waited := now.Sub(j.Created).Minutes() - ran
	rec := &Receipt{ID: id, Project: j.Project, Name: j.Name, Exit: exit, Outcome: outcome, Minutes: round1(ran),
		Declared: j.Minutes, Waited: round1(waited), Charged: round1(j.charged), PeakVRAM: round1(j.peakVRAM),
		PeakRAM: round1(j.peakRAM), Log: log, Ended: now, Requeue: outcome == "evicted" && j.Evict != "force"}
	j.receipt = rec
	j.State = "done"
	d.st.Receipts[id] = rec
	logf("%s: %s (exit %d) after %.1f min, waited %.1f, charged %.1f", j.Name, outcome, exit, ran, waited, j.charged)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

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
	Price    float64       `json:"price,omitempty"` // machine-minute price multiplier of its bid
	Budget   float64       `json:"budget_left"`     // project's machine-minutes left
	Pred     float64       `json:"predicted_minutes,omitempty"`
	Receipt  *Receipt      `json:"receipt,omitempty"`
	Quote    *policy.Quote `json:"quote,omitempty"`
	Market   string        `json:"market,omitempty"` // the quote in one line
	PauseF   string        `json:"pause_file,omitempty"`
	PausedF  string        `json:"paused_file,omitempty"`
	CheckF   string        `json:"checkpoint_file,omitempty"`
}

func (d *Daemon) view(j *job) View {
	now := time.Now()
	a := d.account(j.Project, now)
	v := View{ID: j.ID, Name: j.Name, Project: j.Project, State: j.State, Band: j.Band, Price: policy.Price(d.cfg, j.Bid),
		Budget: round1(math.Max(-1e9, math.Min(1e9, a.Left(d.cfg, d.cfg.Allowance(j.Project))))), Pred: round1(j.PredMinutes),
		Receipt: j.receipt, Paused: !j.pausedAt.IsZero(), Checkpt: !j.checkpoint.IsZero(), Why: j.reason,
		PauseF: PauseFile(j.ID), PausedF: PausedFile(j.ID), CheckF: CheckpointFile(j.ID)}
	switch j.State {
	case "waiting":
		pos, eta, why := d.queueSpot(j, now)
		v.Position, v.ETA, v.Why = pos, eta.Format("15:04"), why
		q := d.quote(*j.Ticket, now)
		v.Quote, v.Market = &q, q.Line()
	case "running":
		v.ETA = j.Started.Add(time.Duration((j.PredMinutes + j.extended) * float64(time.Minute))).Format("15:04")
	}
	return v
}

// queueSpot estimates a waiting ticket's place, start time and what blocks it.
func (d *Daemon) queueSpot(j *job, now time.Time) (int, time.Time, string) {
	order := d.waiting()
	slots := []time.Time{}
	for _, r := range d.running() {
		if r.GPU == j.GPU {
			end := r.Started.Add(time.Duration((r.PredMinutes + r.extended) * float64(time.Minute)))
			if end.Before(now) {
				end = now.Add(time.Minute)
			}
			slots = append(slots, end)
		}
	}
	if len(slots) == 0 {
		slots = []time.Time{now}
	}
	pos := 0
	for _, t := range order {
		if t.GPU != j.GPU {
			continue
		}
		pos++
		// earliest slot
		k := 0
		for i := range slots {
			if slots[i].Before(slots[k]) {
				k = i
			}
		}
		start := slots[k]
		if t.ID == j.ID {
			return pos, start, d.blocker(j)
		}
		slots[k] = start.Add(time.Duration(t.PredMinutes * float64(time.Minute)))
	}
	return pos, now, d.blocker(j)
}

func (d *Daemon) blocker(j *job) string {
	c := d.cfg
	ram, vram := d.headroom(time.Now())
	switch {
	case j.GPU && c.ReserveFor(d.user).PauseGPU && d.user == "game":
		return "the human is gaming (" + d.gameWhy + ")"
	case j.Band == policy.BandSpot:
		return "spot band (over budget or --spot): runs on leftovers only"
	case j.GPU && j.NeedVRAM > vram:
		return fmt.Sprintf("needs %.1f GB VRAM, %.1f GB free after the human's reserve", j.NeedVRAM, math.Max(0, vram))
	case j.NeedRAM > ram:
		return fmt.Sprintf("needs %.1f GB RAM, %.1f GB free after the human's reserve", j.NeedRAM, math.Max(0, ram))
	case j.GPU && len(d.busyOutside()) > 0:
		return "GPU busy outside firefighter: " + strings.Join(d.busyOutside(), ", ")
	case j.GPU:
		return "GPU turn queue"
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
		p := policy.Ticket{Project: "any", GPU: gpu, Minutes: 30}
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
		a := d.account(p, now)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	p.ID, p.Added = NewID(), time.Now()
	d.st.Plans = append(d.st.Plans, &p)
	reply(w, p)
}

func (d *Daemon) unplan(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := r.PathValue("id")
	var keep []*Plan
	for _, p := range d.st.Plans {
		if p.ID != id && p.Project != id {
			keep = append(keep, p)
		}
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
	_ = os.WriteFile(filepath.Join(config.Home(), "STATUS.md"), []byte(b.String()), 0o644)
}
