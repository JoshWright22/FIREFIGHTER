package policy

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
)

// firefighter is the market maker for the machine. Instead of letting agents bid blind against each
// other (LLM bidders start sniping and shading once they see rivals' bids), it posts prices: every
// call gets a quote with the current congestion price, a menu of what each bid level buys (start
// time and cost), and the odds each resource is busy over the next hours. Accepting a posted price
// is the dominant strategy, so there is nothing to game, and agents still see the whole market.

// Congestion is the posted price multiplier for one machine-minute: 0.5 when the resource sits idle
// (fill it up), 1 at balance, up to 4 when demand over the next hour far exceeds what's free.
func Congestion(demandMinutes, supplyMinutes float64) float64 {
	if supplyMinutes <= 0 {
		return 4
	}
	return math.Max(0.5, math.Min(4, 0.5+demandMinutes/supplyMinutes))
}

// Slot is one running or waiting job, as the scheduler simulation sees it.
type Slot struct {
	ID      string
	EndsIn  float64 // running: minutes left (predicted); waiting: predicted minutes
	Running bool
}

// Schedule simulates the GPU (or CPU) line: running jobs finish on their predicted ends, waiting
// jobs start in order on the first free slot. It returns each waiting job's start offset (minutes)
// and the busy intervals of the whole schedule.
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

// BusyProb is the chance the resource is fully taken h minutes from now: the simulated schedule says
// busy or free, blended with how busy this hour of the week usually is (plans and history) so a
// quote never promises more certainty than the past supports.
func BusyProb(h float64, busyUntil []float64, prior float64) float64 {
	sched := 1.0
	for _, b := range busyUntil {
		if b <= h {
			sched = 0 // at least one slot is free by then
			break
		}
	}
	return math.Round((0.8*sched+0.2*prior)*100) / 100
}

// Offer is one line of the menu: what a bid level buys.
type Offer struct {
	Bid      int     `json:"bid"`
	StartsIn float64 `json:"starts_in_min"`
	Position int     `json:"position"`
	Cost     float64 `json:"cost"` // expected machine-minutes charged
}

// Quote is the market information returned with every call.
type Quote struct {
	Resource string             `json:"resource"` // gpu | cpu
	Price    float64            `json:"price"`    // congestion multiplier now
	Busy     map[string]float64 `json:"busy"`     // horizon ("15m","30m","60m","120m") -> probability fully taken
	Menu     []Offer            `json:"menu"`
	Cheapest string             `json:"cheapest_window,omitempty"`
	Advice   string             `json:"advice"`
	Budget   float64            `json:"budget_left"`
}

var MenuBids = []int{1, 3, 5, 7, 10}

// Line renders a quote in one compact line for CLI output and agent context.
func (q Quote) Line() string {
	s := fmt.Sprintf("market %s: price x%.2f, busy 15m %.0f%% 60m %.0f%% 120m %.0f%%", q.Resource, q.Price,
		100*q.Busy["15m"], 100*q.Busy["60m"], 100*q.Busy["120m"])
	for _, o := range q.Menu {
		s += fmt.Sprintf(" | bid %d: %s, cost %.1f", o.Bid, inMin(o.StartsIn), o.Cost)
	}
	if q.Cheapest != "" {
		s += " | cheapest " + q.Cheapest
	}
	return s + " | " + q.Advice
}

func inMin(m float64) string {
	if m < 1 {
		return "now"
	}
	return fmt.Sprintf("~%.0fm", m)
}

// Advise picks the best-value offer for an agent: the cheapest bid that starts within 10% (or 5
// minutes) of the fastest one, unless the budget can't afford it.
func Advise(menu []Offer, budget float64) string {
	if len(menu) == 0 {
		return ""
	}
	fastest := menu[0]
	for _, o := range menu {
		if o.StartsIn < fastest.StartsIn {
			fastest = o
		}
	}
	best := fastest
	for _, o := range menu {
		if o.StartsIn <= fastest.StartsIn+math.Max(5, 0.1*fastest.StartsIn) && o.Cost < best.Cost {
			best = o
		}
	}
	if best.Cost > budget {
		return fmt.Sprintf("budget %.0f is short of %.0f: run --spot or wait for the refill", budget, best.Cost)
	}
	return fmt.Sprintf("best value: bid %d (%s)", best.Bid, inMin(best.StartsIn))
}

// HourOfWeek indexes the 168-slot occupancy history.
func HourOfWeek(t time.Time) int { return int(t.Weekday())*24 + t.Hour() }

// CheapestWindow finds the next start hour (within a day) with the lowest historical occupancy.
func CheapestWindow(now time.Time, history []float64) string {
	if len(history) != 168 {
		return ""
	}
	best, at := 2.0, 0
	for h := 1; h <= 24; h++ {
		if v := history[HourOfWeek(now.Add(time.Duration(h)*time.Hour))]; v < best-0.05 {
			best, at = v, h
		}
	}
	if at == 0 || best > 0.5 {
		return ""
	}
	return now.Add(time.Duration(at)*time.Hour).Format("15:00") + fmt.Sprintf(" (usually %.0f%% busy)", 100*best)
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

// Rank orders a hypothetical ticket among waiting ones at a given bid, for the menu.
func Rank(c config.Config, probe *Ticket, waiting []*Ticket, accts map[string]*Account, capy Capacity, now time.Time) int {
	all := append([]*Ticket{probe}, waiting...)
	for i, t := range Order(c, all, accts, capy, now, nil) {
		if t.ID == probe.ID {
			return i
		}
	}
	return len(waiting)
}
