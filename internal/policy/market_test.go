package policy

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestScheduleOneSlot(t *testing.T) {
	starts, busy := Schedule(1, []Slot{{ID: "r", EndsIn: 10, Running: true}}, []Slot{{ID: "a", EndsIn: 20}, {ID: "b", EndsIn: 5}})
	if starts["a"] != 10 || starts["b"] != 30 || busy[0] != 35 {
		t.Fatalf("starts %v busy %v", starts, busy)
	}
}

func TestScheduleTwoSlots(t *testing.T) {
	starts, _ := Schedule(2, []Slot{{ID: "r", EndsIn: 10, Running: true}}, []Slot{{ID: "a", EndsIn: 20}, {ID: "b", EndsIn: 5}})
	if starts["a"] != 0 || starts["b"] != 10 {
		t.Fatalf("starts %v", starts)
	}
}

func TestBusyProbBlendsSignals(t *testing.T) {
	near := BusyProb(15, []float64{30}, 0, false, false)
	far := BusyProb(15, []float64{300}, 0, false, false)
	if near < 0.8 || near > 0.95 || far < near-0.001 {
		t.Fatalf("scheduled busy: %v %v", near, far)
	}
	if p := BusyProb(60, []float64{30}, 0.5, false, false); p != 0 {
		t.Fatalf("no history known, free by then: %v", p)
	}
	if p := BusyProb(60, []float64{30}, 0.5, true, false); p < 0.25 || p > 0.4 {
		t.Fatalf("history weighs in with the horizon: %v", p)
	}
	if p := BusyProb(60, []float64{30}, 0, false, true); p != 0.7 {
		t.Fatalf("a plan alone: %v", p)
	}
}

func TestCongestionIsExponential(t *testing.T) {
	if Congestion(0, 60) != 0.5 || math.Abs(Congestion(60, 60)-4) > 1e-9 || Congestion(1e6, 60) != 4 || Congestion(1, 0) != 4 {
		t.Fatal("congestion ends")
	}
	if c := Congestion(30, 60); math.Abs(c-0.5*math.Sqrt(8)) > 1e-9 {
		t.Fatalf("half load %v", c)
	}
}

func TestRemainingHeavyTail(t *testing.T) {
	if Remaining(30, 10) != 20 || Remaining(30, 60) != 30 || Remaining(1, 0.5) != 1 {
		t.Fatal("remaining")
	}
}

func quote(normalStart float64) Quote {
	return Quote{Resource: "gpu", Minutes: 20, Price: 1.4, Budget: 240, Refill: 10, Busy: map[string]float64{"15m": 0.9, "60m": 0.4, "120m": 0.1},
		Menu: []Offer{{Tier: "spot", Bid: 5, StartsIn: normalStart + 30, Cost: 10}, {Tier: "normal", Bid: 5, StartsIn: normalStart, Cost: 30}, {Tier: "urgent", Bid: 10, StartsIn: 0, Cost: 60}}}
}

func TestAdviseNeverSaysOutbid(t *testing.T) {
	for _, start := range []float64{0, 20, 90} {
		q := quote(start)
		a := Advise(q, false)
		if strings.Contains(a, "urgent") || strings.Contains(a, "bid") {
			t.Fatalf("advice escalates: %q", a)
		}
	}
	if a := Advise(quote(90), false); !strings.Contains(a, "spot") {
		t.Fatalf("long wait should mention spot: %q", a)
	}
	q := quote(10)
	q.Budget = 5
	if a := Advise(q, false); !strings.Contains(a, "spot") || !strings.Contains(a, "refill") {
		t.Fatalf("short budget: %q", a)
	}
}

func TestQuietWindowSpreadsAgents(t *testing.T) {
	h, n := make([]float64, 168), make([]float64, 168)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	for i := range h {
		h[i], n[i] = 0.9, 10
	}
	for k := 2; k <= 6; k++ {
		h[HourOfWeek(now.Add(time.Duration(k)*time.Hour))] = 0.1
	}
	seen := map[string]bool{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		seen[QuietWindow(now, h, n, nil, id)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every agent got the same quiet hour: %v", seen)
	}
	planned := map[int]bool{}
	for k := 2; k <= 6; k++ {
		planned[HourOfWeek(now.Add(time.Duration(k)*time.Hour))] = true
	}
	if w := QuietWindow(now, h, n, planned, "a"); w != "" {
		t.Fatalf("planned hours must be skipped: %q", w)
	}
	if w := QuietWindow(now, h, make([]float64, 168), nil, "a"); w != "" {
		t.Fatalf("no history must mean no suggestion: %q", w)
	}
}

func TestSurcharge(t *testing.T) {
	if Surcharge([]float64{2, 2, 2}) != 1.25 || Surcharge([]float64{1, 1, 1}) != 1 || Surcharge([]float64{3}) != 1 {
		t.Fatal("surcharge")
	}
}

func TestQuoteLineIsCompact(t *testing.T) {
	q := quote(20)
	q.Do = Advise(q, false)
	l := q.Line()
	if !strings.Contains(l, "start ~20m") || !strings.Contains(l, "do: ") || len(l) > 260 {
		t.Fatalf("%d chars: %s", len(l), l)
	}
}
