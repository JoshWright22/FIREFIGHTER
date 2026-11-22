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
