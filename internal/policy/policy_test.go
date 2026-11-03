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

func TestBidLeadIsBounded(t *testing.T) {
	c := config.Defaults() // max lead 30
	accts := map[string]*Account{"heavy": {Charged: 100}, "light": {Charged: 5}}
	// A bid of 10 buys 30 minutes of lead: not enough to pass a 95-minute gap.
	got := Order(c, []*Ticket{tk("a", "heavy", 10, 10), tk("b", "light", 5, 1)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ba" {
		t.Fatalf("bid 10 jumped too far: %s", ids(got))
	}
	accts["heavy"].Charged = 30 // gap 25 < 30: now the bid wins
	got = Order(c, []*Ticket{tk("a", "heavy", 10, 10), tk("b", "light", 5, 1)}, accts, Capacity{}, t0, nil)
	if ids(got) != "ab" {
		t.Fatalf("bid 10 should win a small gap: %s", ids(got))
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
	r := &ReserveCtl{}
	r.Update(c, "away", 0, t0)
	if r.CPUs != 2 {
		t.Fatalf("away cpus %v", r.CPUs)
	}
	r.Update(c, "active", 9, t0.Add(time.Second)) // busy 9 -> ceil(9)+2 = 11
	if r.CPUs != 11 || r.RAMPct != 10 {
		t.Fatalf("attack must be immediate: %+v", r)
	}
	r.Update(c, "away", 0, t0.Add(2*time.Second))
	if r.CPUs != 11 || r.RAMPct != 10 {
		t.Fatalf("no instant decay: %+v", r)
	}
	r.Update(c, "away", 0, t0.Add(20*time.Second))
	if r.CPUs != 10 {
		t.Fatalf("give back one CPU per 15 s: %v", r.CPUs)
	}
	r.Update(c, "away", 0, t0.Add(70*time.Second))
	if r.RAMPct != 5 {
		t.Fatalf("RAM reserve shrinks after its hold: %v", r.RAMPct)
	}
}
