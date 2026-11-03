package daemon

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
)

// The market maker: posted congestion prices for three tiers, start times from one schedule
// simulation, busy odds from the schedule, plans and history, and one recommended action, priced
// into every answer firefighter gives an agent.

// observeOccupancy keeps an hour-of-week history of how taken the GPU is (any job, foreign compute,
// or the human holding it). Each hour's value moves about a quarter of the way per visit, so it
// reflects the last several weeks, not just the last one; samples counts how much history it has.
func (d *Daemon) observeOccupancy(now time.Time, dt time.Duration) {
	if len(d.st.Occupancy) != 168 {
		d.st.Occupancy = make([]float64, 168)
	}
	if len(d.st.OccSamples) != 168 {
		d.st.OccSamples = make([]float64, 168)
	}
	busy := d.utilEWMA / 100
	if d.gpuHeld() || len(d.busyOutside()) > 0 {
		busy = 1
	}
	for _, j := range d.running() {
		if j.GPU {
			busy = 1
		}
	}
	h := policy.HourOfWeek(now)
	a := math.Min(1, dt.Minutes()/240)
	d.st.Occupancy[h] += a * (busy - d.st.Occupancy[h])
	d.st.OccSamples[h] += dt.Minutes() / 60
}

// slotsFor is how many jobs of a kind run side by side for the schedule simulation.
func (d *Daemon) slotsFor(gpu bool) int {
	if !gpu {
		return 4 // CPU jobs share; RAM admission is the real limit
	}
	if d.cfg.GPU.MaxJobs > 1 && d.utilEWMA < d.cfg.GPU.PackBelowUtil {
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
		left := policy.Remaining(r.PredMinutes+r.extended, r.leaseUsed(now).Minutes())
		running = append(running, policy.Slot{ID: r.ID, EndsIn: left, Running: true})
	}
	if gpu {
		// Unmanaged GPU work: its end is unknown, so assume 30 minutes at first and, past that, as long
		// again as it has already run (heavy tail): a 2-hour training run isn't "done in 30".
		for n, at := range d.outSince {
			running = append(running, policy.Slot{ID: "outside:" + n, EndsIn: policy.Remaining(30, now.Sub(at).Minutes()), Running: true})
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

// planned reports announced GPU work covering the time h minutes from now, and the hours of the
// week announced plans cover (so quiet-window suggestions skip them). When is "now", "HH:MM" or
// anything else (unscheduled: ignored).
func (d *Daemon) planned(now time.Time) (at func(h float64) bool, hours map[int]bool) {
	type span struct{ from, to time.Time }
	var spans []span
	hours = map[int]bool{}
	for _, p := range d.st.Plans {
		if p.GPUPct <= 0 && p.VRAMGB <= 0 {
			continue
		}
		var start time.Time
		w := strings.ToLower(strings.TrimSpace(p.When))
		if w == "now" || w == "" {
			start = p.Added
		} else if t, err := parseClock(w, now.Location()); err == nil {
			start = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if start.Before(now.Add(-time.Duration(math.Max(p.Minutes, 60)) * time.Minute)) {
				start = start.Add(24 * time.Hour)
			}
		} else {
			continue
		}
		end := start.Add(time.Duration(math.Max(p.Minutes, 30)) * time.Minute)
		spans = append(spans, span{start, end})
		for t := start; t.Before(end); t = t.Add(time.Hour) {
			hours[policy.HourOfWeek(t)] = true
		}
	}
	return func(h float64) bool {
		t := now.Add(time.Duration(h) * time.Minute)
		for _, s := range spans {
			if !t.Before(s.from) && t.Before(s.to) {
				return true
			}
		}
		return false
	}, hours
}

// parseClock reads "15:04", "3:04 PM", "3:04pm" or "3pm".
func parseClock(s string, loc *time.Location) (time.Time, error) {
	s = strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(s)), " ", "")
	var err error
	for _, f := range []string{"15:04", "3:04PM", "3PM"} {
		var t time.Time
		if t, err = time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

// quote prices a job against the current market: when each tier would start, what it would cost,
// how busy the resource will be, and one recommended action.
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

	q := policy.Quote{Resource: map[bool]string{true: "gpu", false: "cpu"}[probe.GPU], Minutes: round1(probe.PredMinutes),
		Price: math.Round(price*100) / 100, Busy: map[string]float64{}}
	planAt, planHours := d.planned(now)
	for _, h := range []float64{15, 30, 60, 120} {
		hist, known := 0.0, false
		if len(d.st.Occupancy) == 168 && len(d.st.OccSamples) == 168 {
			i := policy.HourOfWeek(now.Add(time.Duration(h) * time.Minute))
			hist, known = d.st.Occupancy[i], d.st.OccSamples[i] >= 3
		}
		q.Busy[strconv.Itoa(int(h))+"m"] = policy.BusyProb(h, busyUntil, hist, known, probe.GPU && planAt(h))
	}
	a := d.peek(probe.Project, now)
	q.Budget = round1(math.Max(-1e9, math.Min(1e9, a.Left(c, c.Allowance(probe.Project)))))
	if probe.GPU {
		q.Quiet = policy.QuietWindow(now, d.st.Occupancy, d.st.OccSamples, planHours, probe.ID+probe.Project)
	}

	if c.Budget.Policy == "karma" {
		q.Refill = round1(c.Allowance(probe.Project) / 24)
	}
	share := d.share(&probe)
	sur := 1.0
	if h := d.st.History[histKey(&probe)]; h != nil {
		sur = policy.Surcharge(h.Ratios)
	}
	if probe.Created.IsZero() {
		probe.Created = now
	}
	if probe.ID == "" {
		probe.ID = "probe"
	}
	for _, tier := range []string{"spot", "normal", "urgent"} {
		p := probe
		p.Bid, p.Spot = policy.BidNormal, false
		mult := 1.0
		switch tier {
		case "spot":
			p.Spot, p.Band, mult = true, policy.BandSpot, policy.SpotPrice
		case "urgent":
			p.Bid = policy.BidUrgent
		}
		if tier != "spot" {
			p.Band = policy.BandFor(&p, q.Budget > 0) // out of budget, it queues as spot whatever the tier
		}
		pos := policy.Rank(c, &p, waiting, d.st.Accounts, capy, now)
		order := append(append(append([]*policy.Ticket{}, waiting[:pos]...), &p), waiting[pos:]...)
		starts, _ := policy.Schedule(n, running, slots(order))
		start := starts[p.ID]
		if !d.fitsNow(&p) && start < 1 {
			start = 1 // resources short right now even with a free slot
		}
		cost := p.PredMinutes * share * policy.Price(c, p.Bid) * price * sur * mult
		q.Menu = append(q.Menu, policy.Offer{Tier: tier, Bid: p.Bid, StartsIn: round1(start), Position: pos + 1, Cost: round1(cost)})
	}
	q.Do = policy.Advise(q, probe.Bid >= policy.BidUrgent)
	return q
}

// tierOf names a ticket's tier for its own quote line.
func tierOf(t *policy.Ticket) string {
	switch {
	case t.Band == policy.BandSpot:
		return "spot"
	case t.Bid >= policy.BidUrgent:
		return "urgent"
	}
	return "normal"
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

// lockPrice fixes a granted job's per-minute price multiplier: tier price x congestion x surcharge.
// Spot pays a third; work adopted from outside the queue pays 1.5x, so skipping the queue never
// pays off.
func (d *Daemon) lockPrice(j *job, now time.Time) {
	q := d.quote(*j.Ticket, now)
	j.price = policy.Price(d.cfg, j.Bid) * q.Price
	if h := d.st.History[histKey(j.Ticket)]; h != nil {
		j.price *= policy.Surcharge(h.Ratios)
	}
	switch {
	case j.adopted:
		j.price *= 1.5
	case j.Spot:
		j.price *= policy.SpotPrice
	}
}

// peek returns a project's standing without creating an account (quotes and status are read-only).
func (d *Daemon) peek(project string, now time.Time) policy.Account {
	var a policy.Account
	if x := d.st.Accounts[project]; x != nil {
		a = *x
	} else {
		a = d.newAccount(project, now)
	}
	a.Decay(now, d.cfg.Order.HalfLifeHours)
	a.Refill(now, d.cfg, d.cfg.Allowance(project))
	return a
}

// newAccount starts a project that has never run here. It doesn't start at zero use with a full
// budget, which would let an agent jump the queue just by naming a new project: it starts at the
// median use of the existing projects with half a day of budget (the very first project on a
// machine starts full), and earns the rest by refill.
func (d *Daemon) newAccount(project string, now time.Time) policy.Account {
	var used []float64
	for _, a := range d.st.Accounts {
		used = append(used, a.Charged)
	}
	a := policy.Account{DecayAt: now, BucketAt: now, Bucket: d.cfg.Allowance(project)}
	if len(used) > 0 {
		// Later projects start from a pool all new projects share per day (guard.new_project_minutes)
		// and earn the rest by refill: a hundred invented names get no more than one.
		a.Bucket = math.Min(d.cfg.Allowance(project)/2, d.newcomers(now))
		sort.Float64s(used)
		a.Charged, a.Pass = used[len(used)/2], used[len(used)/2]
	}
	return a
}

// newcomers is what's left today in the pool new projects start from.
func (d *Daemon) newcomers(now time.Time) float64 {
	if now.Sub(d.st.NewcomersAt) > 24*time.Hour {
		d.st.Newcomers, d.st.NewcomersAt = d.cfg.Guard.NewProjectMinutes, now
	}
	return math.Max(0, d.st.Newcomers)
}

// probeFromQuery builds a hypothetical ticket from /v1/quote query parameters.
func (d *Daemon) probeFromQuery(r *http.Request) policy.Ticket {
	q := r.URL.Query()
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	t := policy.Ticket{Project: q.Get("project"), GPU: q.Get("gpu") == "1" || q.Get("gpu") == "true",
		Minutes: f("minutes"), VRAMGB: f("vram"), RAMGB: f("ram"), CmdHash: q.Get("cmd"), Bid: policy.BidNormal}
	if q.Get("urgent") == "true" {
		t.Bid = policy.BidUrgent
	}
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
