package policy

import (
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

func TestBusyProb(t *testing.T) {
	if p := BusyProb(15, []float64{30}, 0); p != 0.8 {
		t.Fatalf("scheduled busy: %v", p)
	}
	if p := BusyProb(60, []float64{30}, 0.5); p != 0.1 {
		t.Fatalf("free by then, usually half busy: %v", p)
	}
}

func TestCongestion(t *testing.T) {
	if Congestion(0, 60) != 0.5 || Congestion(60, 60) != 1.5 || Congestion(1e6, 60) != 4 || Congestion(1, 0) != 4 {
		t.Fatal("congestion curve")
	}
}

func TestAdvisePicksCheapestNearFastest(t *testing.T) {
	menu := []Offer{{Bid: 1, StartsIn: 60, Cost: 2}, {Bid: 5, StartsIn: 3, Cost: 10}, {Bid: 10, StartsIn: 0, Cost: 20}}
	if a := Advise(menu, 100); !strings.Contains(a, "bid 5") {
		t.Fatalf("advice %q", a)
	}
	if a := Advise(menu, 5); !strings.Contains(a, "short") {
		t.Fatalf("budget advice %q", a)
	}
}

func TestCheapestWindowAndSurcharge(t *testing.T) {
	h := make([]float64, 168)
	for i := range h {
		h[i] = 0.9
	}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	h[HourOfWeek(now.Add(5*time.Hour))] = 0.1
	if w := CheapestWindow(now, h); !strings.HasPrefix(w, "17:00") {
		t.Fatalf("window %q", w)
	}
	if Surcharge([]float64{2, 2, 2}) != 1.25 || Surcharge([]float64{1, 1, 1}) != 1 || Surcharge([]float64{3}) != 1 {
		t.Fatal("surcharge")
	}
}

func TestQuoteLine(t *testing.T) {
	q := Quote{Resource: "gpu", Price: 1.2, Busy: map[string]float64{"15m": 0.8, "60m": 0.4, "120m": 0.1},
		Menu: []Offer{{Bid: 5, StartsIn: 0, Cost: 30}}, Advice: "best value: bid 5 (now)"}
	if l := q.Line(); !strings.Contains(l, "bid 5: now, cost 30.0") || !strings.Contains(l, "busy 15m 80%") {
		t.Fatal(l)
	}
}
