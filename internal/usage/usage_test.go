package usage

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol*math.Max(1, math.Abs(want)) }

// Buckets are 5% wide: a percentile is never under the samples it stands for, and at most 5% over.
func TestHistPercentiles(t *testing.T) {
	h := NewHist(0.05)
	for i := 1; i <= 100; i++ {
		h.Add(float64(i), 1, t0)
	}
	for _, c := range []struct{ p, want float64 }{{0.5, 50}, {0.9, 90}, {0.98, 98}, {1, 100}} {
		if got := h.Percentile(c.p); got < c.want || got > c.want*1.06 {
			t.Errorf("P%.0f = %.2f, want %.0f to %.0f", c.p*100, got, c.want, c.want*1.06)
		}
	}
	if !near(h.Mean(), 50.5, 0.05) || h.Max() < 100 || h.Max() > 106 {
		t.Errorf("mean %.2f, max %.2f", h.Mean(), h.Max())
	}
	if (&Hist{}).Percentile(0.9) != 0 || (*Hist)(nil).Max() != 0 {
		t.Error("an empty histogram predicts 0")
	}
}

// Autopilot Fig. 2: load 1 for nine time units and 10 for one. By time, P90 is 1; by load, 10: the
// level that covers 90% of the work.
func TestLoadAdjustedPercentile(t *testing.T) {
	h := NewHist(0.05)
	for i := 0; i < 9; i++ {
		h.Add(1, 1, t0)
	}
	h.Add(10, 1, t0)
	if got := h.Percentile(0.9); got > 1.06 {
		t.Errorf("time-based P90 = %.2f, want ~1", got)
	}
	if got := h.LoadPercentile(0.9); got < 10 || got > 10.6 {
		t.Errorf("load-adjusted P90 = %.2f, want ~10", got)
	}
}

// Autopilot Eq. 1: a sample one half-life old counts half.
func TestHistDecay(t *testing.T) {
	h := NewHist(0.05)
	h.Add(8, 1, t0)
	h.Age(t0.Add(12*time.Hour), 12*time.Hour)
	if !near(h.Weight, 0.5, 0.01) {
		t.Fatalf("weight after one half-life = %.3f, want 0.5", h.Weight)
	}
	// An old high value fades out of the percentile as new, lower ones arrive.
	for i := 0; i < 20; i++ {
		h.Age(t0.Add(time.Duration(13+i)*time.Hour), 12*time.Hour)
		h.Add(2, 1, t0.Add(time.Duration(13+i)*time.Hour))
	}
	if got := h.Percentile(0.9); got > 2.2 {
		t.Fatalf("P90 = %.2f: the old peak should have faded", got)
	}
	if h.Max() < 8 {
		t.Fatal("the peak is still remembered while it has weight")
	}
}

func TestPeakNeed(t *testing.T) {
	// No history: the declaration, else the cold-start value.
	if PeakNeed(nil, 6, 3) != 6 || PeakNeed(NewHist(0.05), 0, 3) != 3 {
		t.Fatal("no history: declared, else cold start")
	}
	// One run that peaked at 7.6 GB, declared 1: the next run is admitted on 7.6 x 1.2.
	h := NewHist(0.05)
	AddRun(h, 7.6, RunHalfLifePeak, t0)
	if got := PeakNeed(h, 1, 0); got < 7.6*1.2 || got > 7.6*1.2*1.06 {
		t.Fatalf("after one run need = %.2f, want ~%.2f", got, 7.6*1.2)
	}
	// Warm (3+ runs): P98 of the peaks + 15%, and a declaration far above anything seen is cut to
	// 1.5 x the highest peak.
	AddRun(h, 7.0, RunHalfLifePeak, t0)
	AddRun(h, 7.4, RunHalfLifePeak, t0)
	if got := PeakNeed(h, 0, 0); got < 7.6*1.15 || got > 7.6*1.15*1.06 {
		t.Fatalf("warm need = %.2f, want ~%.2f", got, 7.6*1.15)
	}
	if got := PeakNeed(h, 30, 0); got < 7.6*1.5 || got > 7.6*1.5*1.06 {
		t.Fatalf("over-declared need = %.2f, want ~%.2f (1.5 x the highest peak)", got, 7.6*1.5)
	}
	// Never measured (all zero): the declaration stands.
	z := NewHist(0.05)
	for i := 0; i < 4; i++ {
		AddRun(z, 0, RunHalfLifePeak, t0)
	}
	if PeakNeed(z, 10, 0) != 10 {
		t.Fatal("a resource never measured keeps its declaration")
	}
}

// Runs fade: after a command is slimmed down, its old peak stops deciding its need.
func TestPeakNeedForgetsOldRuns(t *testing.T) {
	h := NewHist(0.05)
	AddRun(h, 12, RunHalfLifePeak, t0)
	for i := 0; i < 60; i++ {
		AddRun(h, 3, RunHalfLifePeak, t0)
	}
	if got := PeakNeed(h, 0, 0); got > 3*1.15*1.06 {
		t.Fatalf("need = %.2f after 60 runs at 3 GB: the 12 GB run should have faded", got)
	}
}

func TestOOMBumpAndRate(t *testing.T) {
	if got := OOMBump(7.6); !near(got, 9.12, 0.001) {
		t.Fatalf("bump(7.6) = %.2f, want 9.12 (x1.2)", got)
	}
	if got := OOMBump(0.2); !near(got, 0.2+100.0/1024, 0.001) {
		t.Fatalf("bump(0.2) = %.3f, want +100 MiB", got)
	}
	h := NewHist(0.05)
	AddRun(h, 2, RunHalfLifeRate, t0)
	AddRun(h, 6, RunHalfLifeRate, t0)
	// Half-life 2 runs: weights 0.707 and 1 -> (2*0.707 + 6)/1.707 = 4.34.
	if got := RateNeed(h, 0); !near(got, 4.34, 0.06) {
		t.Fatalf("mean = %.2f, want ~4.34 (the newer run counts more)", got)
	}
	if RateNeed(nil, 1.5) != 1.5 {
		t.Fatal("no history: the cold value")
	}
}

func TestReserveIsBoundedAndWaitsForHistory(t *testing.T) {
	if Reserve(10, 2, 0.5, 0.15) != 10 {
		t.Fatal("under a day of history the configured value stands")
	}
	if got := Reserve(10, 8, 3, 0.15); !near(got, 9.2, 0.001) {
		t.Fatalf("learned = %.2f, want 9.2 (8 + 15%%)", got)
	}
	if Reserve(10, 1, 3, 0.15) != 5 || Reserve(10, 100, 3, 0.15) != 30 {
		t.Fatal("the learned reserve stays within half to three times the configured one")
	}
}

func TestNSigma(t *testing.T) {
	// mean 5, std 2 -> 5 + 3*2 = 11.
	if got := NSigma([]float64{3, 7, 3, 7, 5, 5, 2.172, 7.828}, 3); !near(got, 11, 0.02) {
		t.Fatalf("mean + 3 sigma = %.2f, want 11", got)
	}
}

// The rise within the horizon: a reading of 10 followed by 14 two minutes later is a rise of 4.
func TestGrowth(t *testing.T) {
	var g Growth
	at := func(min float64) time.Time { return t0.Add(time.Duration(min * float64(time.Minute))) }
	g.Push(at(0), 10, "active", 5*time.Minute)
	g.Push(at(2), 14, "active", 5*time.Minute)
	g.Push(at(4), 11, "present", 5*time.Minute)
	rises, labels := g.Push(at(5), 11, "present", 5*time.Minute)
	if len(rises) != 1 || rises[0] != 4 || labels[0] != "active" {
		t.Fatalf("rises %v labels %v, want [4] [active]", rises, labels)
	}
	rises, _ = g.Push(at(7.5), 9, "present", 5*time.Minute) // the reading at minute 2 (14) leaves: nothing rose above it
	if len(rises) != 1 || rises[0] != 0 {
		t.Fatalf("rises %v, want [0]", rises)
	}
}
