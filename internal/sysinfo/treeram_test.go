package sysinfo

import (
	"math"
	"testing"
)

// nanoTSFM 11/23: a main process and 8 DataLoader workers sharing a 6 GB memory-mapped dataset
// read as 6 GB per process (about 55 GB). Shared pages count once.
func TestTreeRAMCountsSharedPagesOnce(t *testing.T) {
	parts := []ramPart{{own: 2, shared: 6}}
	for i := 0; i < 8; i++ {
		parts = append(parts, ramPart{own: 0.55, shared: 6})
	}
	if got := treeRAM(parts); math.Abs(got-(2+8*0.55+6)) > 1e-9 {
		t.Fatalf("tree RAM %.2f GB, want 12.4", got)
	}
}
