package policy

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
)

// firefighter is the market maker for the machine. Agents don't bid against each other (LLM
// bidders snipe and escalate once they see rivals' positions); firefighter posts prices for three
// tiers (spot, normal, urgent) and tells each agent, in every reply, when each tier would start,
// what it would cost, how busy the machine will be, and one recommended action that never says
// "outbid". Taking a posted price is the dominant strategy, so there is nothing to game.

// Tier bids: spot runs on leftovers at a third of the price; urgent is for work the human is
// waiting on and costs double. Bids between are accepted from the CLI for compatibility.
const (
	BidNormal = 5
	BidUrgent = 10
	SpotPrice = 1.0 / 3
)

// Congestion is the posted price multiplier for one machine-minute: exponential in how committed
// the resource is over the next hour (online primal-dual pricing, Zhang et al. SIGMETRICS'17):
// 0.5 idle, ~1.4 at half load, 4 when fully committed.
func Congestion(demandMinutes, supplyMinutes float64) float64 {
	if supplyMinutes <= 0 {
		return 4
	}
	u := math.Max(0, math.Min(1, demandMinutes/supplyMinutes))
	return 0.5 * math.Pow(8, u)
}

// Remaining estimates a running job's minutes left. Before its prediction: the prediction minus
// its age. Past it: half its age again (heavy-tailed runtimes: a job that overran tends to keep
// going; mean residual life grows with age), never under a minute.
func Remaining(predicted, age float64) float64 {
	// Never less than half its age (heavy tail), so the estimate doesn't drop to ~0 just before the
	// prediction and then jump: it bends smoothly from "prediction minus age" into "half its age".
	return math.Max(1, math.Max(predicted-age, age/2))
}

// Slot is one running or waiting job, as the scheduler simulation sees it.
type Slot struct {
	ID      string
	EndsIn  float64 // running: minutes left (predicted); waiting: predicted minutes
	Running bool
}

// Schedule simulates the GPU (or CPU) line: running jobs finish on their predicted ends, waiting
// jobs start in order on the first free slot. It returns each waiting job's start offset (minutes)
// and the time each slot frees after the whole queue.
func Schedule(slots int, running []Slot, waiting []Slot) (starts map[string]float64, busyUntil []float64) {
	if slots < 1 {
		slots = 1
	}
	free := make([]float64, slots)
	for i, r := range running {
		if i < slots {
			free[i] = math.Max(0, r.EndsIn)
		} else { // more running than slots (foreign or packed): queue behind the earliest
			sort.Float64s(free)
			free[0] += math.Max(0, r.EndsIn)
		}
	}
	starts = map[string]float64{}
	for _, w := range waiting {
		sort.Float64s(free)
		starts[w.ID] = free[0]
		free[0] += math.Max(1, w.EndsIn)
	}
	sort.Float64s(free)
	return starts, free
}

// BusyProb is the chance the resource is fully taken h minutes from now. Three independent signals:
// the simulated schedule (trusted less the further out it looks), how busy this hour of the week
// has been (weighted in as the horizon grows), and announced plans (at a 0.7 reliability):
// P = 1 - (1-Psched)(1-w(h)·Phist)(1-Pplan), w(h) = 1 - e^(-h/60).
func BusyProb(h float64, busyUntil []float64, hist float64, histKnown bool, plan bool) float64 {
	// Each slot frees at an uncertain time around its estimate: the chance it's still taken at h is a
	// log-logistic survival curve with its median at the estimate (runtimes are heavy-tailed, so
	// estimates are soft both ways). The resource is full only while every slot is taken.
	sched := 0.0
	if len(busyUntil) > 0 {
		sched = 0.95
		for _, b := range busyUntil {
			if b <= 0 {
				sched = 0
				break
			}
			sched *= 1 / (1 + math.Pow(h/b, 4))
		}
	}
	w := 0.0
	if histKnown {
		w = 1 - math.Exp(-h/60)
	}
	p := 0.0
	if plan {
		p = 0.7
	}
	return math.Round((1-(1-sched)*(1-w*hist)*(1-p))*100) / 100
}

// Offer is one tier of the menu.
type Offer struct {
	Tier     string  `json:"tier"` // spot | normal | urgent
	Bid      int     `json:"bid"`
	StartsIn float64 `json:"starts_in_min"`
	Position int     `json:"position"`
	Cost     float64 `json:"cost"` // expected machine-minutes charged
}

// Quote is the market information returned with every call.
type Quote struct {
	Resource string             `json:"resource"` // gpu | cpu
	Minutes  float64            `json:"minutes"`  // the job priced (predicted runtime)
	Price    float64            `json:"congestion"`
	Busy     map[string]float64 `json:"busy"` // horizon ("15m","30m","60m","120m") -> probability fully taken
	Menu     []Offer            `json:"menu"`
	Quiet    string             `json:"quiet_window,omitempty"`
	Do       string             `json:"do"`
	Budget   float64            `json:"budget_left"`
	Refill   float64            `json:"refill_per_hour"`
}

// Tier returns the menu entry for a tier (spot, normal, urgent).
func (q Quote) Tier(tier string) (Offer, bool) {
	for _, o := range q.Menu {
		if o.Tier == tier {
			return o, true
		}
	}
	return Offer{}, false
}

// Line renders a quote in one compact line (about 60-80 tokens) for CLI output and agent replies.
func (q Quote) Line() string {
	var b strings.Builder
	n, _ := q.Tier("normal")
	fmt.Fprintf(&b, "%s %.0fm: start %s, cost ~%.0f of %.0f", q.Resource, q.Minutes, inMin(n.StartsIn), n.Cost, q.Budget)
	if u, ok := q.Tier("urgent"); ok && u.StartsIn < n.StartsIn-1 {
		fmt.Fprintf(&b, " (urgent %s ~%.0f)", inMin(u.StartsIn), u.Cost)
	}
	if s, ok := q.Tier("spot"); ok {
		fmt.Fprintf(&b, " (spot %s ~%.0f)", inMin(s.StartsIn), s.Cost)
	}
	fmt.Fprintf(&b, " | busy 15/60/120m %.0f/%.0f/%.0f%%", 100*q.Busy["15m"], 100*q.Busy["60m"], 100*q.Busy["120m"])
	if q.Quiet != "" {
		b.WriteString(" | quiet " + q.Quiet)
	}
	b.WriteString(" | do: " + q.Do)
	return b.String()
}

func inMin(m float64) string {
	if m < 1 {
		return "now"
	}
	if m >= 90 {
		return fmt.Sprintf("~%.1fh", m/60)
	}
	return fmt.Sprintf("~%.0fm", m)
}

// Advise is the one recommended action. It never says "outbid": a normal job takes the normal
// tier; urgent only when the human waits on it (the agent's call, not the market's); spot or a
// quiet window when the wait is long; spot when the budget can't cover it.
func Advise(q Quote, urgent bool) string {
	n, _ := q.Tier("normal")
	s, hasSpot := q.Tier("spot")
	switch {
	case n.Cost > q.Budget && hasSpot:
		return fmt.Sprintf("budget %.0f < %.0f: use spot (~%.0f) or wait for the refill (+%.0f/h)", q.Budget, n.Cost, s.Cost, q.Refill)
	case urgent:
		u, _ := q.Tier("urgent")
		return fmt.Sprintf("urgent starts %s; keep it only if the human is waiting", inMin(u.StartsIn))
	case n.StartsIn >= 60 && q.Quiet != "":
		return fmt.Sprintf("queue it (starts %s), or spot if it can wait, or plan it for %s", inMin(n.StartsIn), strings.Fields(q.Quiet)[0])
	case n.StartsIn >= 60 && hasSpot:
		return fmt.Sprintf("queue it (starts %s); spot is ~%.0f if it can wait", inMin(n.StartsIn), s.Cost)
	case n.StartsIn < 1:
		return "run it now at normal"
	}
	return fmt.Sprintf("queue it at normal (starts %s); keep working meanwhile", inMin(n.StartsIn))
}

// HourOfWeek indexes the 168-slot occupancy history.
func HourOfWeek(t time.Time) int { return int(t.Weekday())*24 + t.Hour() }

// QuietWindow suggests an upcoming quiet hour (within a day) from the occupancy history. To avoid
// herding every agent into the same hour, each job gets its own pick among the near-quietest hours
// (a hash of its id), and hours with announced plans are skipped.
func QuietWindow(now time.Time, history []float64, samples []float64, planned map[int]bool, id string) string {
	if len(history) != 168 || len(samples) != 168 {
		return ""
	}
	type cand struct {
		at int
		v  float64
	}
	var cs []cand
	best := 2.0
	for h := 1; h <= 24; h++ {
		i := HourOfWeek(now.Add(time.Duration(h) * time.Hour))
		if samples[i] < 3 || planned[i] {
			continue // no history yet for that hour, or someone announced work then
		}
		cs = append(cs, cand{h, history[i]})
		best = math.Min(best, history[i])
	}
	if best > 0.5 {
		return ""
	}
	var near []cand
	for _, c := range cs {
		if c.v <= best+0.1 {
			near = append(near, c)
		}
	}
	f := fnv.New32a()
	f.Write([]byte(id))
	c := near[int(f.Sum32())%len(near)]
	return now.Add(time.Duration(c.at)*time.Hour).Format("15:00") + fmt.Sprintf(" (usually %.0f%% busy)", 100*c.v)
}

// Surcharge for a project whose jobs keep running longer than they declare (median ratio > 1.5).
func Surcharge(ratios []float64) float64 {
	if len(ratios) < 3 {
		return 1
	}
	if Predict(1, ratios) > 1.5 {
		return 1.25
	}
	return 1
}

// Rank orders a hypothetical ticket among waiting ones, for the menu.
func Rank(c config.Config, probe *Ticket, waiting []*Ticket, accts map[string]*Account, capy Capacity, now time.Time) int {
	all := append([]*Ticket{probe}, waiting...)
	for i, t := range Order(c, all, accts, capy, now, nil) {
		if t.ID == probe.ID {
			return i
		}
	}
	return len(waiting)
}
