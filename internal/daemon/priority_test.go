package daemon

import (
	"strings"
	"testing"
	"time"
)

// Josh 11/22: a bid-7 job needing 4.5 GB of RAM waited while small bid-3 jobs kept taking the RAM
// as it freed. A lower bid now starts ahead of a waiting higher bid only if it ends before that
// one can start; a higher-bid job with a free share of RAM still goes first.
func TestHigherBidHoldsTheRAMItNeeds(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	// One running job that ends in about 10 minutes (60 predicted, 50 worked) and holds 4 GB.
	d.addRunning("training", 4, now.Add(-50*time.Minute))
	// 7.2 GB free less the 8% (2.56 GB) reserve: 4.64 GB for jobs, 1.6 GB margin on top of a need.
	d.snap.RAMAvailGB = 7.2
	big := d.addWaiting("proxy fx-infer-views8rd", 4.5, 90, 10*time.Minute)
	big.Bid = 7
	small := d.addWaiting("infer dumps s2", 1, 30, 2*time.Minute)
	small.Bid, small.GPU = 3, false
	quick := d.addWaiting("quick look", 1, 5, time.Minute)
	quick.Bid, quick.GPU = 3, false
	d.admit(now)
	if big.State != "waiting" {
		t.Fatalf("the bid-7 job can't fit yet: %s", big.State)
	}
	if small.State != "waiting" {
		t.Fatalf("a 30-minute bid-3 job took the RAM a waiting bid-7 job needs")
	}
	if why := d.blocker(small); !strings.Contains(why, "a bid-7 job") || !strings.Contains(why, "ends before then") {
		t.Fatalf("blocker: %s", why)
	}
	if quick.State != "granted" {
		t.Fatalf("a 5-minute job that ends before the bid-7 one can start should pass it: %s (%s)", quick.State, d.blocker(quick))
	}
	// Equal bids keep plain backfill.
	same := d.addWaiting("same bid", 0.5, 30, time.Minute)
	same.Bid, same.GPU = 7, false
	d.admit(now)
	if same.State != "granted" {
		t.Fatalf("an equal bid that fits still starts beside it: %s (%s)", same.State, d.blocker(same))
	}
}

// A lower-bid GPU job never takes the card a higher-bid GPU job waits for (here on VRAM), unless
// it ends before the card frees.
func TestLowerBidKeepsOffTheGPUAHigherBidWaitsFor(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.cfg.GPU.MaxJobs = 2
	run := d.addRunning("training", 2, now.Add(-50*time.Minute)) // ends in about 10 minutes
	d.snap.GPUs[0].UsedGB = 5
	run.vramGB = 4
	d.snap.RAMAvailGB = 20
	big := d.addWaiting("big model", 2, 90, 10*time.Minute)
	big.Bid, big.NeedVRAM = 7, 10.5 // 16 GB card, 5 used, 1 reserved: 10 left until the training ends
	small := d.addWaiting("small eval", 1, 30, time.Minute)
	small.Bid, small.NeedVRAM = 3, 2
	d.admit(now)
	if big.State != "waiting" || small.State != "waiting" {
		t.Fatalf("big %s, small %s: the bid-3 job must not take the card the bid-7 job waits for", big.State, small.State)
	}
	if why := d.blocker(small); !strings.Contains(why, "GPU slot") {
		t.Fatalf("blocker: %s", why)
	}
	small.Minutes = 5 // ends before the card frees: it may use the time
	d.admit(now)
	if small.State != "granted" {
		t.Fatalf("a job that ends first should pass: %s (%s)", small.State, d.blocker(small))
	}
}

// Josh 11/22: five bid-7 jobs waited behind one overdue 6.5 GB job with 4.6 GB free and no running
// job that would give it the rest ("start not forecast"). A head whose start can't be foreseen
// reserves nothing (until it has waited 2 x max_wait); one that can be foreseen lets through only
// jobs that end before it starts. A lower-bid head never holds a higher bid.
func TestBackfillHeadsHoldOnlyWhatTheyCanUse(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.RAMAvailGB = 2.56 + 4.6 // 4.6 GB for jobs beside the 8% reserve
	d.cfg.Reserve.RAMMarginPct = 0
	head := d.addWaiting("clean single-13m", 6.5, 110, 50*time.Minute) // overdue, start unknown
	head.Bid = 7
	next := d.addWaiting("proxy fx-infer-views8rd", 4, 90, 10*time.Minute)
	next.Bid = 7
	d.admit(now)
	if head.State != "waiting" || next.State != "granted" {
		t.Fatalf("head %s, next %s: a fitting job must start when the head's start can't be foreseen (%s)", head.State, next.State, d.blocker(next))
	}

	// A lower-bid overdue head doesn't hold a higher bid.
	d2 := testDaemon(t)
	d2.snap.RAMAvailGB = 2.56 + 4.6
	d2.cfg.Reserve.RAMMarginPct = 0
	low := d2.addWaiting("bid-6 head", 6.5, 110, 3*time.Hour)
	low.Bid = 6
	high := d2.addWaiting("bid-8 job", 4, 90, time.Minute)
	high.Bid = 8
	if got := d2.position(high, now); got != 1 {
		t.Fatalf("the bid-8 job must be #1, got #%d", got)
	}
	d2.admit(now)
	if high.State != "granted" {
		t.Fatalf("a bid-8 job waited behind a bid-6 head: %s", d2.blocker(high))
	}
}
