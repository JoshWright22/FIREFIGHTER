package daemon

import (
	"strings"
	"testing"
	"time"
)

// fdf58d99 (11/23): a PyTorch job's cache grew to 13.8 GB of a 16.3 GB card while the human's apps
// held 1.7 GB, so VRAM fell under the human's 1 GB reserve and the job was stopped 13 times in a
// row. Its share is the card less the reserve less everything else on it; the stop says how to cap.
func TestVRAMShareAndTheStopSayHowToCap(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.GPUs[0].VRAMGB, d.snap.GPUs[0].UsedGB = 16.3, 1.7+13.8
	d.res.VRAMGB = 1
	j := d.addRunning("FINAL", 4, now.Add(-5*time.Minute))
	j.vramGB, j.device = 13.8, 0
	capGB, total := d.vramCap(j)
	if capGB < 13.55 || capGB > 13.65 || total != 16.3 {
		t.Fatalf("share %.2f of %.1f GB, want 13.6 of 16.3", capGB, total)
	}
	v := d.view(j)
	if v.VRAMCapGB != capGB || v.VRAMTotalGB != 16.3 {
		t.Fatalf("view: cap %.1f total %.1f", v.VRAMCapGB, v.VRAMTotalGB)
	}
	// Under the reserve long enough: the job is asked to stop, and told how to stay under its share.
	d.vramLow = now.Add(-time.Hour)
	d.ladder(now)
	if !strings.Contains(j.reason, "set_per_process_memory_fraction") || !strings.Contains(j.reason, "may hold 13.6 GB") {
		t.Fatalf("stop reason: %q", j.reason)
	}
}

// 5658b8b4: a job stopped for VRAM waited out a doubling back-off (up to 30 min) with no way to
// try again after its owner capped it; cancel and resubmit was the only way out. retry ends the
// wait now and keeps the count.
func TestRetryEndsTheBackOff(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addWaiting("FINAL", 1, 60, time.Minute)
	if d.backoff == nil {
		d.backoff = map[string]backoff{}
	}
	d.backoff[j.ID] = backoff{n: 5, at: now.Add(-time.Minute), why: "VRAM below the human's 1.0 GB reserve"}
	if d.backoffLeft(j.ID, now) <= 0 {
		t.Fatal("setup: it should be backing off")
	}
	b := d.backoff[j.ID]
	b.at = time.Time{}
	d.backoff[j.ID] = b
	if d.backoffLeft(j.ID, now) != 0 || d.backoff[j.ID].n != 5 {
		t.Fatalf("after retry: left %v, count %d", d.backoffLeft(j.ID, now), d.backoff[j.ID].n)
	}
	if dec := d.waitingDecision(j, now); dec.Kind == blockBackoff {
		t.Fatalf("still held by the back-off: %s", dec.Text)
	}
}
