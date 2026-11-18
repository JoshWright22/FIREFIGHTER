package harvest

import (
	"os"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func tou() Profit {
	return Profit{CentsPerKWh: 10, EarnPerHour: 0.125, CutPct: 0, Periods: []Period{
		{Months: "5-10", Days: "mon-fri", From: "16:00", To: "19:00", Cents: 36},
		{Months: "11-4", Days: "mon-fri", From: "16:00", To: "19:00", Cents: 30},
		{Months: "11-12,1-4", From: "10:00", To: "15:00", Cents: 4},
		{From: "22:00", To: "06:00", Cents: 7},
	}}
}

func TestPrice(t *testing.T) {
	p := tou()
	for _, c := range []struct {
		when string
		want float64
	}{
		{"2026-11-17 17:00", 30}, // Tue, winter peak
		{"2026-07-14 16:00", 36}, // Tue, summer peak
		{"2026-07-14 19:00", 10}, // To is exclusive
		{"2026-11-21 17:00", 10}, // Saturday
		{"2026-11-17 12:00", 4},  // winter midday
		{"2026-07-14 12:00", 10},
		{"2026-11-17 23:30", 7}, // wraps past midnight
		{"2026-11-18 03:00", 7},
	} {
		if got := p.Price(at(c.when)); got != c.want {
			t.Errorf("%s: %v, want %v", c.when, got, c.want)
		}
	}
}

func TestDecidePeakLoses(t *testing.T) {
	b := &Book{Dir: t.TempDir()}
	p := tou()
	if c := b.Decide(p, at("2026-11-17 17:00"), 500); c.OK {
		t.Fatalf("500 W at 30¢ (15¢/h) against 12.5¢/h earned must not run: %+v", c)
	} else if !strings.Contains(c.Why, "would net") {
		t.Fatalf("why: %s", c.Why)
	}
	if c := b.Decide(p, at("2026-11-17 12:00"), 400); !c.OK {
		t.Fatalf("400 W at 4¢ should pay: %+v", c)
	}
	p.CutPct = 70 // 3.75¢/h kept vs 4¢/h of power at 10¢
	if c := b.Decide(p, at("2026-11-17 08:00"), 400); c.OK {
		t.Fatalf("the cut makes it lose: %+v", c)
	}
}

func TestTripAndStale(t *testing.T) {
	dir := t.TempDir()
	b := &Book{Dir: dir}
	p := tou()
	p.MinHours = 10
	start := at("2026-11-02 00:00")
	for i := 0; i < 30*60; i++ { // 30 h at 300 W, 10¢
		b.Accrue(start.Add(time.Duration(i)*time.Minute), time.Minute, 300, 250, 10)
	}
	// 30 h * 0.3 kW * $0.10 = $0.90 of power
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	b2 := Open(dir)
	if len(b2.Hours) != 30 {
		t.Fatalf("reloaded %d hours", len(b2.Hours))
	}
	now := at("2026-11-04 12:00")
	if c := b2.Decide(p, now, 300); !c.OK {
		t.Fatalf("no earnings yet, not stale: should run: %s", c.Why)
	}
	if c := b2.Decide(p, at("2026-11-12 12:00"), 300); c.OK || !strings.Contains(c.Why, "no earnings reported") {
		t.Fatalf("stale: %+v", c)
	}
	if err := AddEarning(dir, Earning{Through: at("2026-11-03 06:00"), USD: 0.50}); err != nil {
		t.Fatal(err)
	}
	b2.LoadEarnings()
	c := b2.Decide(p, now, 300)
	if !c.Trip || c.OK {
		t.Fatalf("$0.50 earned vs $0.90 power should trip: %+v", c)
	}
	os.Remove(EarningsPath(dir))
	_ = AddEarning(dir, Earning{Through: at("2026-11-03 06:00"), USD: 3})
	b2.earnMod = time.Time{}
	b2.LoadEarnings()
	c = b2.Decide(p, now, 300)
	if c.Trip || !c.OK || !c.Measured {
		t.Fatalf("$3 vs $0.90 should run on the measured rate: %+v", c)
	}
}

func TestWallWatts(t *testing.T) {
	p := Profit{CPUWatts: 7.5, ExtraWatts: 10, PSUEff: 0.9}
	if w := WallWatts(p, 260, 2); w < 316 || w > 317 { // (260 + 2*7.5 + 10) / 0.9
		t.Fatalf("%v", w)
	}
}
