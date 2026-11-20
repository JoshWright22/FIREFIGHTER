package sysinfo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// VM is the memory of the local Linux VM (WSL2, where Docker Desktop's engine also runs). The host
// counts everything it has handed the VM as used, yet most of it is often file cache from a build
// or an install, which the guest gives back within seconds when told to. That cache is reclaimable
// memory, not anyone's need: no job is stopped for it.
type VM struct {
	Name    string  `json:"name,omitempty"` // the distro the numbers were read in
	HostGB  float64 `json:"host_gb"`        // what the host has handed the VM (its vmmem working set)
	UsedGB  float64 `json:"used_gb"`        // what the programs inside it really use
	CacheGB float64 `json:"cache_gb"`       // file cache it gives back on request (never more than HostGB)
	Guest   bool    `json:"guest"`          // UsedGB and CacheGB were read from the guest (only while RAM is tight)
}

// fake is FIREFIGHTER_FAKE_FILE: scripted readings for tests (nil when unset or unreadable).
type fake struct {
	Idle       *float64 `json:"idle"`
	Foreground *string  `json:"foreground"`
	RAMAvailGB *float64 `json:"ram_avail_gb"`
	NetMbps    *float64 `json:"net_mbps"`
	GPUs       []GPU    `json:"gpus"`
	CommitFree *float64 `json:"commit_free_gb"` // scripted commit headroom
	VMHostGB   *float64 `json:"vm_host_gb"`     // a scripted WSL VM: what the host gave it
	VMCacheGB  *float64 `json:"vm_cache_gb"`    // and how much of that is cache
	DiskFreeGB *float64 `json:"disk_free_gb"`   // free space on every drive
}

func readFake() *fake {
	path := os.Getenv("FIREFIGHTER_FAKE_FILE")
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f fake
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return &f
}

// faked says whether readings are scripted: the real VM is then never touched.
func faked() bool { return os.Getenv("FIREFIGHTER_FAKE_FILE") != "" }

// AvailGB is the RAM available right now (one reading, outside the regular sample).
func AvailGB() float64 {
	if f := readFake(); f != nil && f.RAMAvailGB != nil {
		return *f.RAMAvailGB
	}
	if v, err := mem.VirtualMemory(); err == nil {
		return gb(v.Available)
	}
	return 0
}

// VMHost is how much RAM the host has handed the local Linux VM (0: none running). Cheap: it reads
// the process list and never wakes the VM.
func VMHost() float64 {
	if faked() {
		if f := readFake(); f != nil && f.VMHostGB != nil {
			return *f.VMHostGB
		}
		return 0
	}
	return vmHost()
}

// VMGuest reads the VM's own memory numbers. It runs a command inside the VM (which keeps an idle
// distro awake), so callers ask only while RAM is tight.
func VMGuest(hostGB float64) (VM, bool) {
	if faked() {
		f := readFake()
		if f == nil || f.VMCacheGB == nil {
			return VM{}, false
		}
		return VM{Name: "fake", HostGB: hostGB, CacheGB: *f.VMCacheGB, UsedGB: max(0, hostGB-*f.VMCacheGB), Guest: true}, true
	}
	name, info, ok := vmGuest()
	if !ok {
		return VM{}, false
	}
	return vmFrom(name, hostGB, info), true
}

// vmFrom turns the guest's /proc/meminfo into what it uses and what it would give back. Shared
// memory (tmpfs) is counted as cache by Linux but can't be dropped. The host may already have taken
// part of the cache back (trimmed or paged out), so the reclaimable part is bounded by what the
// host still holds beyond the guest's real use.
func vmFrom(name string, hostGB float64, meminfo string) VM {
	kb := map[string]float64{}
	for _, line := range strings.Split(meminfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if f := strings.Fields(v); ok && len(f) > 0 {
			kb[k] = num(f[0])
		}
	}
	const g = 1 << 20 // kB per GB
	cache := max(0, kb["Cached"]+kb["Buffers"]-kb["Shmem"]) / g
	used := max(0, kb["MemTotal"]-kb["MemFree"])/g - cache
	return VM{Name: name, HostGB: hostGB, UsedGB: used, CacheGB: max(0, min(cache, hostGB-used)), Guest: true}
}

// ReclaimVM has the VM drop its file cache and compact its free memory, so the pages go back to the
// host. Programs inside keep running; they only re-read what they need from disk.
func ReclaimVM(name string) error {
	if faked() {
		// Tests script the effect: the request is recorded next to the fake file.
		f, err := os.OpenFile(os.Getenv("FIREFIGHTER_FAKE_FILE")+".reclaim", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, time.Now().Format(time.RFC3339))
		return err
	}
	return vmReclaim(name)
}

// Disk is one drive's space.
type Disk struct {
	Path    string  `json:"path"` // the drive or mount point
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
}

// DiskOf is the drive a path is on, with its free space.
func DiskOf(path string) (Disk, bool) {
	if path == "" {
		return Disk{}, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Disk{}, false
	}
	root := volume(abs)
	u, err := disk.Usage(root)
	if err != nil || u.Total == 0 {
		return Disk{}, false
	}
	d := Disk{Path: root, TotalGB: gb(u.Total), FreeGB: gb(u.Free)}
	if f := readFake(); f != nil && f.DiskFreeGB != nil {
		d.FreeGB = *f.DiskFreeGB
	}
	return d, true
}
