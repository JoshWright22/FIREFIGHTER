package daemon

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
)

// The market maker: posted congestion prices, a menu per bid level, and occupancy odds, priced into
// every answer firefighter gives an agent.

// observeOccupancy keeps an hour-of-week history of how taken the GPU is (any job, foreign compute,
// or the human gaming), so quotes know which hours are usually free.
func (d *Daemon) observeOccupancy(now time.Time, dt time.Duration) {
	if len(d.st.Occupancy) != 168 {
		d.st.Occupancy = make([]float64, 168)
	}
	busy := d.snap.GPUUtil() / 100
	if d.gpuHeld() || len(d.busyOutside()) > 0 {
		busy = 1
	}
	for _, j := range d.running() {
		if j.GPU {
			busy = 1
		}
	}
	h := policy.HourOfWeek(now)
	a := math.Min(1, dt.Minutes()/30) // ~30 minutes of samples make up most of an hour's estimate
	d.st.Occupancy[h] += a * (busy - d.st.Occupancy[h])
}

// slotsFor is how many jobs of a kind run side by side for the schedule simulation.
func (d *Daemon) slotsFor(gpu bool) int {
	if !gpu {
		return 4 // CPU jobs share; RAM admission is the real limit
	}
	if d.cfg.GPU.MaxJobs > 1 && d.snap.GPUUtil() < d.cfg.GPU.PackBelowUtil {
		return d.cfg.GPU.MaxJobs
	}
	return 1
}

// line returns the running and waiting slots of one kind (GPU or CPU), excluding one ticket id.
func (d *Daemon) line(gpu bool, exclude string, now time.Time) (running []policy.Slot, waiting []*policy.Ticket) {
	for _, r := range d.running() {
		if r.GPU != gpu || r.ID == exclude {
			continue
		}
		left := r.PredMinutes + r.extended - now.Sub(r.Started).Minutes()
		running = append(running, policy.Slot{ID: r.ID, EndsIn: math.Max(1, left), Running: true})
	}
	if gpu {
		for range d.busyOutside() { // unmanaged GPU work: unknown end, assume 30 min
			running = append(running, policy.Slot{ID: "outside", EndsIn: 30, Running: true})
		}
		if d.gpuHeld() {
			running = append(running, policy.Slot{ID: "human", EndsIn: math.Max(15, time.Until(d.gameTill).Minutes()), Running: true})
		}
	}
	for _, t := range d.waiting() {
		if t.GPU == gpu && t.ID != exclude {
			waiting = append(waiting, t)
		}
	}
	return
}

func slots(ts []*policy.Ticket) []policy.Slot {
	out := make([]policy.Slot, len(ts))
	for i, t := range ts {
		out[i] = policy.Slot{ID: t.ID, EndsIn: t.PredMinutes}
	}
	return out
}

// quote prices a job (probe) against the current market. The probe's Bid is varied for the menu.
func (d *Daemon) quote(probe policy.Ticket, now time.Time) policy.Quote {
	c := d.cfg
	capy := d.capacity()
	running, waiting := d.line(probe.GPU, probe.ID, now)
	n := d.slotsFor(probe.GPU)
	_, busyUntil := policy.Schedule(n, running, slots(waiting))

	// Congestion over the next hour: committed minutes of this kind versus slot-minutes available.
	demand := 0.0
	for _, r := range running {
		demand += math.Min(60, r.EndsIn)
	}
	for _, w := range waiting {
		demand += math.Min(60, w.PredMinutes)
	}
	price := policy.Congestion(demand, 60*float64(n))

	q := policy.Quote{Resource: map[bool]string{true: "gpu", false: "cpu"}[probe.GPU], Price: math.Round(price*100) / 100, Busy: map[string]float64{}}
	prior := 0.0
	for _, h := range []float64{15, 30, 60, 120} {
		if len(d.st.Occupancy) == 168 {
			prior = d.st.Occupancy[policy.HourOfWeek(now.Add(time.Duration(h)*time.Minute))]
		}
		q.Busy[strconv.Itoa(int(h))+"m"] = policy.BusyProb(h, busyUntil, prior)
	}
	if probe.GPU {
		q.Cheapest = policy.CheapestWindow(now, d.st.Occupancy)
	}

	a := d.account(probe.Project, now)
	q.Budget = round1(math.Max(-1e9, math.Min(1e9, a.Left(c, c.Allowance(probe.Project)))))
	share := d.share(&probe)
	h := d.st.History[histKey(&probe)]
	sur := 1.0
	if h != nil {
		sur = policy.Surcharge(h.Ratios)
	}
	if probe.Created.IsZero() {
		probe.Created = now
	}
	if probe.ID == "" {
		probe.ID = "probe"
	}
	for _, b := range policy.MenuBids {
		p := probe
		p.Bid = b
		pos := policy.Rank(c, &p, waiting, d.st.Accounts, capy, now)
		order := append(append(append([]*policy.Ticket{}, waiting[:pos]...), &p), waiting[pos:]...)
		starts, _ := policy.Schedule(n, running, slots(order))
		start := starts[p.ID]
		if !d.fitsNow(&p) && start < 1 {
			start = 1 // resources short right now even with a free slot
		}
		cost := p.PredMinutes * share * policy.Price(c, b) * price * sur
		q.Menu = append(q.Menu, policy.Offer{Bid: b, StartsIn: round1(start), Position: pos + 1, Cost: round1(cost)})
	}
	q.Advice = policy.Advise(q.Menu, q.Budget)
	return q
}

// share is a job's dominant share of the machine per minute (DRF): what one minute of it costs.
func (d *Daemon) share(t *policy.Ticket) float64 {
	capy := d.capacity()
	s := math.Max(t.NeedRAM/capy.RAMGB, 0.05)
	if t.GPU {
		s = math.Max(s, 1)
		if capy.VRAMGB > 0 {
			s = math.Max(s, t.NeedVRAM/capy.VRAMGB)
		}
	} else {
		s = math.Max(s, 0.25) // a typical CPU job keeps ~a quarter of the cores busy
	}
	return s
}

// fitsNow says whether RAM and VRAM headroom admit the job this moment.
func (d *Daemon) fitsNow(t *policy.Ticket) bool {
	ram, vram := d.headroom(time.Now())
	_, vt := d.snap.MaxFreeVRAM()
	if t.NeedRAM > ram {
		return false
	}
	return !t.GPU || vt == 0 || t.NeedVRAM <= vram
}

// lockPrice fixes a granted job's per-minute price multiplier: bid price x congestion x surcharge.
func (d *Daemon) lockPrice(j *job, now time.Time) {
	q := d.quote(*j.Ticket, now)
	j.price = policy.Price(d.cfg, j.Bid) * q.Price
	if h := d.st.History[histKey(j.Ticket)]; h != nil {
		j.price *= policy.Surcharge(h.Ratios)
	}
}

// probeFromQuery builds a hypothetical ticket from /v1/quote query parameters.
func (d *Daemon) probeFromQuery(r *http.Request) policy.Ticket {
	q := r.URL.Query()
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	t := policy.Ticket{Project: q.Get("project"), GPU: q.Get("gpu") == "1" || q.Get("gpu") == "true",
		Minutes: f("minutes"), VRAMGB: f("vram"), RAMGB: f("ram"), CmdHash: q.Get("cmd")}
	if t.Project == "" {
		t.Project = "default"
	}
	d.fillNeeds(&t)
	return t
}

func (d *Daemon) quoteHandler(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reply(w, d.quote(d.probeFromQuery(r), time.Now()))
}
