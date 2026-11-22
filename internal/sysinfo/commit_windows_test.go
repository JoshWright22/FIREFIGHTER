//go:build windows

package sysinfo

import "testing"

// 2026-11-22: a system-managed page file on a 1.8 TB drive with 198 GB free may grow to 3x RAM;
// a fixed one stops at its maximum; a nearly full drive bounds both.
func TestPagefileMax(t *testing.T) {
	drive := func(free, size float64) func(string) (float64, float64, bool) {
		return func(string) (float64, float64, bool) { return free, size, true }
	}
	if got := pagefileEntryMax(`C:\pagefile.sys 0 0`, 31.6, drive(198, 1862)); got < 94 || got > 95 {
		t.Fatalf("system-managed: %.1f GB, want 3 x 31.6", got)
	}
	if got := pagefileEntryMax(`?:\pagefile.sys`, 31.6, drive(198, 1862)); got < 94 || got > 95 {
		t.Fatalf("automatic on every drive: %.1f GB", got)
	}
	if got := pagefileEntryMax(`C:\pagefile.sys 4096 16384`, 31.6, drive(198, 1862)); got != 16 {
		t.Fatalf("fixed maximum: %.1f GB, want 16", got)
	}
	if got := pagefileEntryMax(`C:\pagefile.sys 0 0`, 31.6, drive(15, 1862)); got != 5 {
		t.Fatalf("15 GB free: %.1f GB, want 5 (10 kept)", got)
	}
}
