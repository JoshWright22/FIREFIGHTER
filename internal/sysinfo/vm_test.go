package sysinfo

import (
	"math"
	"testing"
)

// /proc/meminfo of the docker-desktop WSL distro on 2026-11-13, after a Docker image build: 24 GB
// VM, 13.5 GB of it file cache, under half a GB really used by programs.
const meminfo = `MemTotal:       24735632 kB
MemFree:         8870864 kB
MemAvailable:   23460412 kB
Buffers:          136556 kB
Cached:         14191140 kB
SwapCached:            4 kB
Shmem:             29900 kB
SReclaimable:     607436 kB
`

func TestVMCacheIsToldFromUse(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 0.05 }
	// The host still holds all of it: the whole cache is reclaimable.
	vm := vmFrom("docker-desktop", 19, meminfo)
	if !near(vm.CacheGB, 13.63) || !near(vm.UsedGB, 1.50) || !vm.Guest {
		t.Fatalf("vm = %+v, want 13.6 GB cache and 1.5 GB used", vm)
	}
	// The host already took part back (working set 10.4 GB): only what it still holds beyond the
	// guest's real use can come back.
	vm = vmFrom("docker-desktop", 10.44, meminfo)
	if !near(vm.CacheGB, 8.94) {
		t.Fatalf("cache = %.2f GB, want 8.94 (10.44 held - 1.50 used)", vm.CacheGB)
	}
	if vm = vmFrom("x", 1, meminfo); vm.CacheGB != 0 {
		t.Fatalf("cache = %.2f GB, want 0 when the host holds less than the guest uses", vm.CacheGB)
	}
}

func TestDiskOfFindsTheDrive(t *testing.T) {
	d, ok := DiskOf(t.TempDir())
	if !ok || d.TotalGB <= 0 || d.FreeGB <= 0 || d.FreeGB > d.TotalGB || d.Path == "" {
		t.Fatalf("DiskOf = %+v, %v", d, ok)
	}
}
