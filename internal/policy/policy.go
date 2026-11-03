// Package policy is firefighter's scheduling math with no I/O: who goes next, what a turn costs,
// how budgets refill, and how big the human's reserve is right now. Sources for each rule are in
// docs/research.md.
package policy

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
)

// Bands: the human (band 0) is not a ticket and is never in any market.
const (
	BandShort = 1 // short CPU jobs: admitted when they fit, never auctioned
	BandBatch = 2 // in-budget GPU and long jobs: the market
	BandSpot  = 3 // over budget, past the lease, or --spot: leftovers only, preempted first
)

type Ticket struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	Cwd       string    `json:"cwd"`
	Command   string    `json:"command"`
	CmdHash   string    `json:"cmd_hash"`
	GPU       bool      `json:"gpu"`
	VRAMGB    float64   `json:"vram_gb"` // declared
	RAMGB     float64   `json:"ram_gb"`  // declared
	Minutes   float64   `json:"minutes"` // declared lease
	Bid       int       `json:"bid"`
	Evict     string    `json:"evict"` // never | graceful | force
	Spot      bool      `json:"spot"`
	Band      int       `json:"band"`
	State     string    `json:"state"` // waiting | running | done
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started"`
	PID       int       `json:"pid"`
	ClientPID int       `json:"client_pid"`
	Device    *int      `json:"gpu_device,omitempty"` // re-registration: the card it was placed on
	Vendor    string    `json:"gpu_vendor,omitempty"`

	NeedVRAM    float64 `json:"need_vram_gb"` // admission need: max(declared, learned peak x margin)
	NeedRAM     float64 `json:"need_ram_gb"`
	PredMinutes float64 `json:"pred_minutes"` // declared minutes corrected by this command's history
}

// Account is one project's standing. Everything is in machine-minutes: one minute of a whole GPU, or
// of all of any one resource (DRF's dominant share).
type Account struct {
	Charged  float64   `json:"charged"` // decayed, priced: the las key
	Pass     float64   `json:"pass"`    // never decays: the stride key
	GPU      float64   `json:"gpu"`     // decayed per-resource use: the drf key
	VRAM     float64   `json:"vram"`
	RAM      float64   `json:"ram"`
	CPU      float64   `json:"cpu"`
	DecayAt  time.Time `json:"decay_at"`
	Bucket   float64   `json:"bucket"` // karma credits left
	BucketAt time.Time `json:"bucket_at"`
	Day      string    `json:"day"` // daily budgets
	UsedDay  float64   `json:"used_day"`
	Fresh    bool      `json:"fresh"`
	Created  time.Time `json:"created,omitempty"` // when the project first ran here
}

// Capacity of the machine, for DRF shares.
type Capacity struct {
	CPUs   float64
	RAMGB  float64
	VRAMGB float64
}

// Decay halves attained service every HalfLifeHours (Slurm-style fair share, Tiresias LAS).
func (a *Account) Decay(now time.Time, halfLifeHours float64) {
	if a.DecayAt.IsZero() || halfLifeHours <= 0 {
		a.DecayAt = now
		return
	}
	f := math.Pow(0.5, now.Sub(a.DecayAt).Hours()/halfLifeHours)
	a.Charged *= f
	a.GPU *= f
	a.VRAM *= f
	a.RAM *= f
	a.CPU *= f
	a.DecayAt = now
}

// Refill tops up the budget. karma: a steady drip of the daily allowance, banked up to BankDays
// (Karma, OSDI'23: unused share becomes credit). daily: resets at local midnight.
func (a *Account) Refill(now time.Time, c config.Config, allowance float64) {
	switch c.Budget.Policy {
	case "karma":
		cap := allowance * math.Max(c.Budget.BankDays, 1)
		if a.BucketAt.IsZero() {
			a.Bucket, a.BucketAt = allowance, now
			return
		}
		a.Bucket = math.Min(cap, a.Bucket+allowance*now.Sub(a.BucketAt).Minutes()/1440)
		a.BucketAt = now
	case "daily":
		if d := now.Format("2006-01-02"); d != a.Day {
			a.Day, a.UsedDay = d, 0
		}
	}
}

// Left is the budget remaining in machine-minutes (+Inf with budget.policy = none).
func (a *Account) Left(c config.Config, allowance float64) float64 {
	switch c.Budget.Policy {
	case "karma":
		return a.Bucket
	case "daily":
		return allowance - a.UsedDay
	}
	return math.Inf(1)
}

// Charge books machine-minutes of use. price scales it (paid urgency); the per-resource minutes feed DRF.
func (a *Account) Charge(c config.Config, gpu, vram, ram, cpu, price float64) {
	base := math.Max(math.Max(gpu, vram), math.Max(ram, cpu))
	a.GPU += gpu
	a.VRAM += vram
	a.RAM += ram
	a.CPU += cpu
	a.Charged += base * price
	a.Pass += base * price
	a.UsedDay += base * price
	if c.Budget.Policy == "karma" {
		a.Bucket -= base * price
	}
}

// Price of a bid: 5 is list price, 10 costs double, 1 costs a fifth (Tycoon-style pay per use).
// Free pricing makes every bid cost the same, so bids then only break ties.
func Price(c config.Config, bid int) float64 {
	if c.Pricing.Policy == "free" {
		return 1
	}
	return float64(clampBid(bid)) / 5
}

// Lead is how many machine-minutes of attained service a bid subtracts from its key: a bid can jump
// the queue by a bounded amount but never starve anyone.
func Lead(c config.Config, bid int) float64 {
	return float64(clampBid(bid)-5) / 5 * c.Pricing.MaxLeadMinutes
}

func clampBid(b int) int {
	if b < 1 {
		return 1
	}
	if b > 10 {
		return 10
	}
	return b
}

// Key is a ticket's sort key under the configured order policy; lower goes first.
func Key(c config.Config, t *Ticket, a *Account, capy Capacity, weight float64) float64 {
	if weight <= 0 {
		weight = 1
	}
	switch c.Order.Policy {
	case "las":
		return a.Charged/weight - Lead(c, t.Bid)
	case "stride":
		return a.Pass/weight - Lead(c, t.Bid)
	case "drf": // per-resource use is already in machine-minutes (fractions of capacity)
		return math.Max(math.Max(a.GPU, a.VRAM), math.Max(a.RAM, a.CPU)) / weight
	case "srpt":
		return t.PredMinutes
	}
	return float64(t.Created.UnixNano()) // fifo
}

// Order sorts waiting tickets: band first, then anything past max_wait, then the policy key, then
// shortest predicted, then oldest. Lottery draws the order by tickets = weight x bid price.
func Order(c config.Config, waiting []*Ticket, accts map[string]*Account, capy Capacity, now time.Time, rng *rand.Rand) []*Ticket {
	out := append([]*Ticket(nil), waiting...)
	overdue := func(t *Ticket) bool {
		return c.Order.MaxWaitMinutes > 0 && now.Sub(t.Created).Minutes() >= c.Order.MaxWaitMinutes
	}
	if c.Order.Policy == "lottery" {
		return lottery(c, out, now, rng, overdue)
	}
	keys := map[string]float64{}
	for _, t := range out {
		a := accts[t.Project]
		if a == nil {
			a = &Account{}
		}
		keys[t.ID] = Key(c, t, a, capy, c.Weight(t.Project))
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Band != b.Band {
			return a.Band < b.Band
		}
		if oa, ob := overdue(a), overdue(b); oa != ob {
			return oa
		} else if oa {
			return a.Created.Before(b.Created)
		}
		if ka, kb := keys[a.ID], keys[b.ID]; ka != kb {
			return ka < kb
		}
		if a.PredMinutes != b.PredMinutes {
			return a.PredMinutes < b.PredMinutes
		}
		return a.Created.Before(b.Created)
	})
	return out
}

func lottery(c config.Config, ts []*Ticket, now time.Time, rng *rand.Rand, overdue func(*Ticket) bool) []*Ticket {
	if rng == nil {
		rng = rand.New(rand.NewSource(now.UnixNano()))
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Band != ts[j].Band {
			return ts[i].Band < ts[j].Band
		}
		return overdue(ts[i]) && !overdue(ts[j])
	})
	var out []*Ticket
	for len(ts) > 0 {
		// Draw only within the leading band (and overdue tickets always win their draw).
		n := 1
		for n < len(ts) && ts[n].Band == ts[0].Band && overdue(ts[n]) == overdue(ts[0]) {
			n++
		}
		total := 0.0
		for _, t := range ts[:n] {
			total += c.Weight(t.Project) * Price(c, t.Bid)
		}
		pick, x := 0, rng.Float64()*total
		for i, t := range ts[:n] {
			if x -= c.Weight(t.Project) * Price(c, t.Bid); x <= 0 {
				pick = i
				break
			}
		}
		out = append(out, ts[pick])
		ts = append(ts[:pick:pick], ts[pick+1:]...)
	}
	return out
}

// Predict corrects a declared runtime with this command's history (Tsafrir et al.: user estimates are
// poor, recent actual/declared ratios are a good fix). ratios are the last few actual/declared.
func Predict(minutes float64, ratios []float64) float64 {
	if minutes <= 0 {
		minutes = 30
	}
	if len(ratios) == 0 {
		return minutes
	}
	r := append([]float64(nil), ratios...)
	sort.Float64s(r)
	return minutes * r[len(r)/2]
}

// BandFor places a new ticket.
func BandFor(t *Ticket, inBudget bool) int {
	switch {
	case t.Spot || !inBudget:
		return BandSpot
	case !t.GPU && t.Minutes > 0 && t.Minutes <= 3:
		return BandShort
	}
	return BandBatch
}

// ---- the human ------------------------------------------------------------------------------

// UserState classifies the human from input idle time and whether something game-like is in front.
func UserState(c config.Config, idleSeconds float64, game bool) string {
	switch {
	case game:
		return "game"
	case idleSeconds < c.Reserve.ActiveSeconds:
		return "active"
	case idleSeconds < c.Reserve.AwaySeconds:
		return "present"
	}
	return "away"
}

// ReserveCtl sizes the human's reserve: it grows in the same tick demand appears and gives it back
// slowly (fast attack, slow decay: PerfIso's idle-core buffer, BOINC's 15-20 s hold).
type ReserveCtl struct {
	CPUs      float64
	RAMPct    float64
	VRAMGB    float64
	cpuStep   time.Time
	ramHold   time.Time
	vramHold  time.Time
	GameUntil time.Time
}

// Update moves the reserve toward target. busyNonJobCPUs is how many CPUs the human and other
// non-job programs are using right now; in active and game states the CPU reserve also covers that
// plus 2 spare, so a compile or a game always finds idle cores.
func (r *ReserveCtl) Update(c config.Config, state string, busyNonJobCPUs float64, now time.Time) {
	t := c.ReserveFor(state)
	cpu := float64(t.CPUs)
	if state == "active" || state == "game" {
		cpu = math.Max(cpu, math.Ceil(busyNonJobCPUs)+2)
	}
	if cpu >= r.CPUs {
		r.CPUs, r.cpuStep = cpu, now
	} else if now.Sub(r.cpuStep).Seconds() >= c.Reserve.CPUShrinkSeconds {
		r.CPUs, r.cpuStep = math.Max(cpu, r.CPUs-1), now
	}
	if t.RAMPct >= r.RAMPct {
		r.RAMPct, r.ramHold = t.RAMPct, now
	} else if now.Sub(r.ramHold).Seconds() >= c.Reserve.RAMShrinkSeconds {
		r.RAMPct = t.RAMPct
	}
	if t.VRAMGB >= r.VRAMGB {
		r.VRAMGB, r.vramHold = t.VRAMGB, now
	} else if now.Sub(r.vramHold).Seconds() >= c.Reserve.VRAMShrinkSeconds {
		r.VRAMGB = t.VRAMGB
	}
}
