package policy

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func tk(id, project string, bid int, ageMin float64) *Ticket {
	return &Ticket{ID: id, Project: project, Bid: bid, Band: BandBatch, GPU: true, Created: t0.Add(-time.Duration(ageMin * float64(time.Minute))), PredMinutes: 10}
}

func ids(ts []*Ticket) string {
	s := ""
	for _, t := range ts {
		s += t.ID
	}
	return s
}

func TestLASPrefersLeastServedProject(t *testing.T) {
	c := config.Defaults()
	accts := map[string]*Account{"heavy": {Charged: 100}, "light": {Charged: 5}}
	got := Order(c, []*Ticket{tk("a", "heavy", 5, 10), tk("b", "light", 5, 1)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ba" {
		t.Fatalf("las order = %s, want ba", ids(got))
	}
}

func TestBidOrdersTheLine(t *testing.T) {
	c := config.Defaults()
	accts := map[string]*Account{"heavy": {Charged: 100}, "light": {Charged: 5}}
	// Josh 11/22: bid-7 GPU jobs of a much-served project waited behind a lightly served
	// project's bid-5 jobs. The bid orders the line; fairness (the las key) orders equal bids.
	got := Order(c, []*Ticket{tk("b", "light", 5, 1), tk("a", "heavy", 7, 10)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ab" {
		t.Fatalf("bid 7 must go before bid 5: %s", ids(got))
	}
	got = Order(c, []*Ticket{tk("a", "heavy", 5, 10), tk("b", "light", 5, 1)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ba" {
		t.Fatalf("equal bids: the least-served project first: %s", ids(got))
	}
}

func TestBackgroundWaitsBehindProjectWork(t *testing.T) {
	c := config.Defaults()
	bot, work := tk("b", "hub-apply", 5, 60), tk("w", "nanoTSFM", 2, 0) // the bot overdue, the work bid low
	bot.Background = true
	for _, pol := range config.OrderPolicies {
		c.Order.Policy = pol
		got := Order(c, []*Ticket{bot, work}, map[string]*Account{}, Capacity{}, t0, rand.New(rand.NewSource(1)))
		if ids(got) != "wb" {
			t.Fatalf("%s: a background job must wait behind project work, even overdue: %s", pol, ids(got))
		}
	}
	if Priority(bot) != BidLow {
		t.Fatalf("background priority %d", Priority(bot))
	}
	bot.Bid = BidUrgent
	if Priority(bot) != BidUrgent {
		t.Fatal("an urgent background job keeps its bid")
	}
}

func TestMaxWaitBeatsKey(t *testing.T) {
	c := config.Defaults()
	accts := map[string]*Account{"heavy": {Charged: 1000}, "light": {}}
	got := Order(c, []*Ticket{tk("a", "heavy", 1, 50), tk("b", "light", 10, 1)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ab" {
		t.Fatalf("a waited past max_wait and must go first: %s", ids(got))
	}
}

func TestBandsBeforeKeys(t *testing.T) {
	c := config.Defaults()
	a, b := tk("a", "x", 10, 60), tk("b", "y", 1, 0)
	a.Band = BandSpot
	got := Order(c, []*Ticket{a, b}, map[string]*Account{}, Capacity{}, t0, nil)
	if ids(got) != "ba" {
		t.Fatalf("spot must come after batch even when overdue: %s", ids(got))
	}
}

func TestEveryPolicyOrdersEverything(t *testing.T) {
	for _, p := range config.OrderPolicies {
		c := config.Defaults()
		c.Order.Policy = p
		ts := []*Ticket{tk("a", "p1", 3, 5), tk("b", "p2", 7, 4), tk("c", "p3", 5, 3)}
		got := Order(c, ts, map[string]*Account{"p1": {Charged: 3, Pass: 3, GPU: 3}}, Capacity{CPUs: 16, RAMGB: 32, VRAMGB: 16}, t0, rand.New(rand.NewSource(1)))
		if len(got) != 3 {
			t.Fatalf("%s lost tickets: %d", p, len(got))
		}
		seen := map[string]bool{}
		for _, x := range got {
			seen[x.ID] = true
		}
		if len(seen) != 3 {
			t.Fatalf("%s duplicated tickets: %s", p, ids(got))
		}
	}
}

func TestFIFOAndSRPT(t *testing.T) {
	c := config.Defaults()
	c.Order.Policy = "fifo"
	a, b := tk("a", "x", 5, 1), tk("b", "y", 5, 5)
	if got := Order(c, []*Ticket{a, b}, nil, Capacity{}, t0, nil); ids(got) != "ba" {
		t.Fatalf("fifo: %s", ids(got))
	}
	c.Order.Policy = "srpt"
	a.PredMinutes, b.PredMinutes = 2, 50
	if got := Order(c, []*Ticket{a, b}, nil, Capacity{}, t0, nil); ids(got) != "ab" {
		t.Fatalf("srpt: %s", ids(got))
	}
}

func TestDRFUsesDominantShare(t *testing.T) {
	c := config.Defaults()
	c.Order.Policy = "drf"
	capy := Capacity{CPUs: 16, RAMGB: 32, VRAMGB: 16}
	// p1 used little GPU but half the RAM for a minute (dominant 0.5); p2 used GPU 0.3.
	accts := map[string]*Account{"p1": {GPU: 0.1, RAM: 0.5}, "p2": {GPU: 0.3}}
	if got := Order(c, []*Ticket{tk("a", "p1", 5, 1), tk("b", "p2", 5, 1)}, accts, capy, t0, nil); ids(got) != "ba" {
		t.Fatalf("drf: %s", ids(got))
	}
}

func TestLotteryFollowsWeights(t *testing.T) {
	c := config.Defaults()
	c.Order.Policy = "lottery"
	c.Budget.Weights = map[string]float64{"big": 9, "small": 1}
	rng := rand.New(rand.NewSource(42))
	wins := 0
	for i := 0; i < 2000; i++ {
		got := Order(c, []*Ticket{tk("a", "big", 5, 1), tk("b", "small", 5, 1)}, nil, Capacity{}, t0, rng)
		if got[0].ID == "a" {
			wins++
		}
	}
	if share := float64(wins) / 2000; math.Abs(share-0.9) > 0.03 {
		t.Fatalf("lottery share %.3f, want ~0.9", share)
	}
}

func TestPriceSchedule(t *testing.T) {
	c := config.Defaults()
	for bid, want := range map[int]float64{1: 0.2, 5: 1, 10: 2, 99: 2, -3: 0.2} {
		if got := Price(c, bid); got != want {
			t.Errorf("price(%d)=%v want %v", bid, got, want)
		}
	}
	c.Pricing.Policy = "free"
	if Price(c, 10) != 1 {
		t.Error("free pricing must cost list price")
	}
}

func TestKarmaRefillsAndBanks(t *testing.T) {
	c := config.Defaults() // 240/day, bank 2 days
	a := &Account{}
	a.Refill(t0, c, 240)
	if a.Bucket != 240 {
		t.Fatalf("starts full: %v", a.Bucket)
	}
	a.Charge(c, 100, 0, 0, 0, 1)
	if a.Bucket != 140 {
		t.Fatalf("spend: %v", a.Bucket)
	}
	a.Refill(t0.Add(6*time.Hour), c, 240) // a quarter day = 60
	if math.Abs(a.Bucket-200) > 1e-6 {
		t.Fatalf("drip: %v", a.Bucket)
	}
	a.Refill(t0.Add(10*24*time.Hour), c, 240)
	if a.Bucket != 480 {
		t.Fatalf("bank cap is 2 days: %v", a.Bucket)
	}
}

func TestDailyBudgetResets(t *testing.T) {
	c := config.Defaults()
	c.Budget.Policy = "daily"
	a := &Account{}
	a.Refill(t0, c, 100)
	a.Charge(c, 80, 0, 0, 0, 1)
	if a.Left(c, 100) != 20 {
		t.Fatalf("left %v", a.Left(c, 100))
	}
	a.Refill(t0.Add(24*time.Hour), c, 100)
	if a.Left(c, 100) != 100 {
		t.Fatalf("reset: %v", a.Left(c, 100))
	}
}

func TestDecayHalvesInHalfLife(t *testing.T) {
	a := &Account{Charged: 100, DecayAt: t0}
	a.Decay(t0.Add(12*time.Hour), 12)
	if math.Abs(a.Charged-50) > 1e-9 {
		t.Fatalf("decay: %v", a.Charged)
	}
}

func TestChargeIsDominantResource(t *testing.T) {
	c := config.Defaults()
	a := &Account{}
	a.Charge(c, 0.2, 0.5, 0.1, 0.3, 2)
	if a.Charged != 1.0 {
		t.Fatalf("max(0.2,0.5,0.1,0.3)*2 = 1, got %v", a.Charged)
	}
}

func TestPredictUsesMedianRatio(t *testing.T) {
	if p := Predict(10, []float64{1, 3, 2}); p != 20 {
		t.Fatalf("predict %v", p)
	}
	if p := Predict(0, nil); p != 30 {
		t.Fatalf("default %v", p)
	}
}

func TestBandFor(t *testing.T) {
	if BandFor(&Ticket{GPU: false, Minutes: 2}, true) != BandShort {
		t.Error("short CPU job should be band 1")
	}
	if BandFor(&Ticket{GPU: true, Minutes: 2}, true) != BandBatch {
		t.Error("GPU job should be band 2")
	}
	if BandFor(&Ticket{GPU: true}, false) != BandSpot {
		t.Error("over budget should be spot")
	}
	if BandFor(&Ticket{Spot: true}, true) != BandSpot {
		t.Error("--spot should be spot")
	}
}

func TestUserState(t *testing.T) {
	c := config.Defaults()
	cases := map[string]struct {
		idle float64
		game bool
	}{"game": {500, true}, "active": {2, false}, "present": {60, false}, "away": {600, false}}
	for want, in := range cases {
		if got := UserState(c, in.idle, in.game); got != want {
			t.Errorf("%v -> %s want %s", in, got, want)
		}
	}
}

func TestReserveFastAttackSlowDecay(t *testing.T) {
	c := config.Defaults()
	c.Reserve.Auto = false // the fixed reserve; under reserve.auto light use returns at once (daemon tests)
	r := &ReserveCtl{}
	r.Update(c, "away", 0, 16, t0)
	if r.CPUs != 2 {
		t.Fatalf("away cpus %v", r.CPUs)
	}
	r.Update(c, "active", 9, 16, t0.Add(time.Second)) // busy 9 -> ceil(9)+2 = 11
	if r.CPUs != 11 || r.RAMPct != 10 {
		t.Fatalf("attack must be immediate: %+v", r)
	}
	r.Update(c, "away", 0, 16, t0.Add(2*time.Second))
	if r.CPUs != 11 || r.RAMPct != 10 {
		t.Fatalf("no instant decay: %+v", r)
	}
	r.Update(c, "away", 0, 16, t0.Add(20*time.Second))
	if r.CPUs != 10 {
		t.Fatalf("give back one CPU per 15 s: %v", r.CPUs)
	}
	r.Update(c, "away", 0, 16, t0.Add(70*time.Second))
	if r.RAMPct != 5 {
		t.Fatalf("RAM reserve shrinks after its hold: %v", r.RAMPct)
	}
}

// Other programs' load grows the CPU reserve only up to reserve.max_cpu_pct: a 13-CPU Docker build
// on a 16-CPU machine leaves jobs 4 CPUs, not 1.
func TestReserveFollowsLoadOnlyUpToItsCap(t *testing.T) {
	c := config.Defaults()
	r := &ReserveCtl{}
	r.Update(c, "active", 13, 16, t0)
	if r.CPUs != 12 {
		t.Fatalf("reserve = %v CPUs, want 12 (75%% of 16)", r.CPUs)
	}
	r = &ReserveCtl{}
	r.Update(c, "game", 15, 4, t0) // a small machine: the state's own base still applies
	if r.CPUs != float64(c.Reserve.Game.CPUs) {
		t.Fatalf("game reserve on 4 CPUs = %v, want the base %d", r.CPUs, c.Reserve.Game.CPUs)
	}
	c.Reserve.MaxCPUPct = 0 // off: the old unbounded rule
	r = &ReserveCtl{}
	r.Update(c, "active", 13, 16, t0)
	if r.CPUs != 15 {
		t.Fatalf("uncapped reserve = %v, want 15", r.CPUs)
	}
}

// A project never owes more than a day of its allowance, and a refund undoes a charge.
func TestDebtLimitAndRefund(t *testing.T) {
	c := config.Defaults() // karma, 240 a day
	a := &Account{Bucket: 100}
	a.Charge(c, 600, 0, 0, 0, 1) // one long run priced at 600
	a.LimitDebt(c, 240)
	if a.Bucket != -240 {
		t.Fatalf("debt = %.0f, want it held at -240 (one day)", a.Bucket)
	}
	a = &Account{Bucket: 100}
	a.Charge(c, 18, 0, 0, 0, 1)
	a.Refund(c, 18)
	if a.Bucket != 100 || a.Charged != 0 || a.Pass != 0 {
		t.Fatalf("after a refund: %+v", a)
	}
}
