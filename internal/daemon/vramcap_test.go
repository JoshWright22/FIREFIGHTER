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
