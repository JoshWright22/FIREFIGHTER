package main

// End-to-end RAM and disk pressure, including VM cache reclaim.

import (
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

// RAM under the human's reserve with a job that holds real memory. Where a suspended job's RAM can
// be moved out (Windows) it is suspended and resumes when RAM is back: nothing is lost and nothing
// is requeued. Elsewhere it is asked to checkpoint and exit, and is put back in line.
func TestRAMPressureSuspendsOrCheckpoints(t *testing.T) {
	v, _ := mem.VirtualMemory()
	total := float64(v.Total) / (1 << 30)
	e := newEnv(t, nil)
	e.extra = []string{"FF_HOG_MB=600"}
	e.ff("config", "set", "ladder.pressure_seconds", "1")
	e.ff("config", "set", "ladder.kill_below_ram_pct", "0.01")
	e.ff("config", "set", "reserve.ram_shrink_seconds", "2") // settle quickly after the shortage
	// A fixed reserve: learning would read the scripted drop in free RAM as the human's own use,
	// grow the reserve, and leave a shortfall the job can't cover (flaky on macOS runners).
	e.ff("config", "set", "reserve.learn", "false")
	log := filepath.Join(e.home, "evict.txt")
	done := make(chan string, 1)
	go func() {
		out, _ := e.job("pausable:20:"+log, "--minutes", "1", "--name", "evictee")
		done <- out
	}()
	eventually(t, "job running", 15*time.Second, func() bool {
		st := e.status()
		r, _ := st["running"].([]any)
		return len(r) == 1
	})
	// Its RAM has been measured (macOS reports a process's memory later than Linux and Windows; the
	// test used to drop free RAM before the guard had seen the job's 600 MB).
	eventually(t, "the job's RAM measured", 30*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		if len(r) != 1 {
			return false
		}
		m, _ := r[0].(map[string]any)
		gb, _ := m["ram_now_gb"].(float64)
		return gb >= 0.4
	})
	// 0.3 GB under the floor kept for the human, as the guard reports it (reserve.auto sizes it for
	// basic use). A shortfall the job's 0.6 GB can cover.
	m, _ := e.status()["machine"].(map[string]any)
	floor, _ := m["ram_reserve_gb"].(float64)
	if floor <= 0 {
		floor = math.Max(total*0.08, math.Min(4, total*0.15))
	}
	e.setFake(map[string]any{"idle": 9999.0, "foreground": "", "ram_avail_gb": floor - 0.3})
	if runtime.GOOS == "windows" {
		eventually(t, "suspended and its RAM freed", 20*time.Second, func() bool { return strings.Contains(e.log(), "suspending it freed") })
		if out, _ := e.ff("status"); !strings.Contains(out, "suspended, not stopped") {
			t.Fatalf("status doesn't say the job is suspended:\n%s", out)
		}
		before := readFile(log)
		time.Sleep(2 * time.Second)
		if readFile(log) != before || strings.Contains(before, "checkpointed") {
			t.Fatalf("a suspended job kept running or was told to exit:\n%s", readFile(log))
		}
	} else {
		eventually(t, "checkpoint", 20*time.Second, func() bool { return strings.Contains(readFile(log), "checkpointed") })
		time.Sleep(1500 * time.Millisecond)
	}
	e.setFake(map[string]any{"idle": 9999.0, "foreground": ""})
	select {
	case out := <-done:
		if !strings.Contains(readFile(log), "finished") {
			t.Fatalf("job didn't finish:\n%s\n%s", readFile(log), out)
		}
		if runtime.GOOS == "windows" {
			if strings.Contains(out, "back in line") || !strings.Contains(out, "suspended 1 time(s)") || strings.Contains(e.log(), "killed") {
				t.Fatalf("a suspended job should resume where it stopped, and its receipt should say so:\n%s\n%s", out, e.log())
			}
		} else if !strings.Contains(out, "back in line") || !strings.Contains(out, "floor kept for the human") {
			t.Fatalf("evicted job should be told why and requeue:\n%s", out)
		}
	case <-time.After(120 * time.Second):
		t.Fatalf("job never finished:\n%s", e.log())
	}
}

// Windows free RAM is critically low because the WSL/Docker VM is full of file cache. The guard has
// the VM give the cache back and touches no job, and a job waiting for RAM says what it waits for.
// Learn the helper command first so first-run pacing cannot mask the RAM gate.
func TestVMCacheIsReclaimedNotJobs(t *testing.T) {
	v, _ := mem.VirtualMemory()
	total := float64(v.Total) / (1 << 30)
	e := newEnv(t, nil)
	if out, code := e.job("sleep:1500:"+filepath.Join(e.home, "warmup.txt"), "--minutes", "1", "--name", "learn helper"); code != 0 {
		t.Fatalf("warming up the helper: %s", out)
	}
	e.extra = []string{"FF_HOG_MB=600"}
	e.ff("config", "set", "ladder.pressure_seconds", "1")
	e.ff("config", "set", "reserve.ram_shrink_seconds", "2")
	log := filepath.Join(e.home, "run.txt")
	done := make(chan string, 1)
	go func() {
		out, _ := e.job("pausable:8:"+log, "--minutes", "1", "--name", "scoring")
		done <- out
	}()
	eventually(t, "job running", 15*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		return len(r) == 1
	})
	e.setFake(map[string]any{"vm_host_gb": 19.0, "vm_cache_gb": 18.0}) // a VM full of cache, RAM still fine
	eventually(t, "the VM shows in status", 15*time.Second, func() bool {
		out, _ := e.ff("status")
		return strings.Contains(out, "WSL/Docker VM: holds 19.0 GB")
	})
	e.setFake(map[string]any{"ram_avail_gb": total * 0.015, "vm_host_gb": 19.0, "vm_cache_gb": 18.0}) // under the 3% kill line
	// The guard must have read the low RAM before the waiter is submitted: submitting admits at once
	// against the guard's last reading (a slow ARM runner granted it on the stale one).
	eventually(t, "the guard sees the low RAM", 15*time.Second, func() bool {
		m, _ := e.status()["machine"].(map[string]any)
		avail, ok := m["ram_avail_gb"].(float64)
		return ok && avail <= total*0.02
	})
	waiter := e.detach("exit:0", "--minutes", "1", "--name", "waiter")
	if w := e.mustWaitState(waiter); !strings.Contains(w, "RAM") && !strings.Contains(w, "file cache") {
		t.Fatalf("the waiting job doesn't say it waits for RAM:\n%s", w)
	}
	marker := e.fake + ".reclaim"
	eventually(t, "the VM asked to drop its cache", 20*time.Second, func() bool { return exists(marker) })
	e.setFake(map[string]any{"vm_host_gb": 2.0, "vm_cache_gb": 0.5}) // the cache came back to Windows
	select {
	case out := <-done:
		l := e.log()
		if strings.Contains(l, "killed") || strings.Contains(l, "suspended") || strings.Contains(l, "checkpoint and exit") ||
			strings.Contains(out, "back in line") || !strings.Contains(readFile(log), "finished") {
			t.Fatalf("the job was touched for RAM the VM could give back:\n%s\n%s", out, l)
		}
		if !strings.Contains(l, "give back its 18.0 GB of file cache before any job is touched") {
			t.Fatalf("the log doesn't record the reclaim:\n%s", l)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("job never finished:\n%s", e.log())
	}
	if out, code := e.ff("wait", waiter, "--timeout", "60"); code != 0 {
		t.Fatalf("the waiting job never ran once RAM was back:\n%s", out)
	}
}

// A drive under the disk reserve: jobs wait and say why, and start once there is space.
func TestDiskReserveHoldsNewJobs(t *testing.T) {
	e := newEnv(t, map[string]any{"disk_free_gb": 2.0})
	e.ff("config", "set", "reserve.disk_free_gb", "10")
	e.job("exit:0", "--minutes", "1", "--name", "probe") // may start before the first disk reading
	time.Sleep(6 * time.Second)                          // the drives are read every 5 s
	id := e.detach("exit:0", "--minutes", "1", "--name", "writer")
	eventually(t, "waiting for disk", 15*time.Second, func() bool { return strings.Contains(e.mustWaitState(id), "under the") })
	if out, _ := e.ff("status"); !strings.Contains(out, "GB kept for the human") {
		t.Fatalf("status doesn't show the disk reserve:\n%s", out)
	}
	e.setFake(map[string]any{"disk_free_gb": 200.0})
	if out, code := e.ff("wait", id, "--timeout", "60"); code != 0 {
		t.Fatalf("job never ran with space back:\n%s", out)
	}
}

// A download frozen for a game that then meets a RAM shortage is never killed outright: where its
// RAM can be moved out (Windows) it stays frozen with its RAM freed; elsewhere it is thawed by the
// RAM pressure rule and asked to save (it checkpoints and is requeued).
func TestFrozenDownloadSavesUnderRAMPressure(t *testing.T) {
	v, _ := mem.VirtualMemory()
	total := float64(v.Total) / (1 << 30)
	e := newEnv(t, map[string]any{"net_mbps": 0.0})
	e.extra = []string{"FF_HOG_MB=600"}
	e.ff("config", "set", "ladder.suspend_max_seconds", "1")
	e.ff("config", "set", "ladder.pressure_seconds", "5")
	e.ff("config", "set", "ladder.kill_below_ram_pct", "0.01")
	e.ff("config", "set", "reserve.game_cool_seconds", "1")
	e.ff("config", "set", "reserve.ram_shrink_seconds", "2")
	e.ff("config", "set", "reserve.learn", "false") // a fixed reserve: learning reads the scripted drop as the human's and grows the shortfall past the job (flaky on macOS)
	log := filepath.Join(e.home, "frozen.txt")
	done := make(chan string, 1)
	go func() {
		out, _ := e.job("pausable:40:"+log, "--net", "--minutes", "1", "--name", "download")
		done <- out
	}()
	eventually(t, "download running", 90*time.Second, func() bool { // after the 30 s measuring gap
		r, _ := e.status()["running"].([]any)
		return len(r) == 1
	})
	// Its 600 MB measured before it is frozen (CI 2026-11-22: frozen one second in, at 0.3 GB, no
	// job's RAM covered the shortfall and nothing was asked to save).
	eventually(t, "the download's RAM measured", 30*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		if len(r) != 1 {
			return false
		}
		m, _ := r[0].(map[string]any)
		gb, _ := m["ram_now_gb"].(float64)
		return gb >= 0.4
	})
	e.setFake(map[string]any{"idle": 0.0, "foreground": "game.exe", "net_mbps": 0.0})
	eventually(t, "frozen for the game", 20*time.Second, func() bool { out, _ := e.ff("status"); return strings.Contains(out, "gaming") })
	time.Sleep(3 * time.Second) // past suspend_max_seconds
	// 0.3 GB under the game state's 15% reserve.
	e.setFake(map[string]any{"idle": 0.0, "foreground": "game.exe", "ram_avail_gb": total*0.15 - 0.3, "net_mbps": 0.0})
	if runtime.GOOS == "windows" {
		eventually(t, "its RAM freed while frozen", 30*time.Second, func() bool { return strings.Contains(e.log(), "suspending it freed") })
		if strings.Contains(readFile(log), "checkpointed") || strings.Contains(e.log(), "killed") {
			t.Fatalf("a frozen download was stopped instead of having its RAM moved out:\n%s", e.log())
		}
	} else if runtime.GOOS == "darwin" {
		// macOS compresses a stopped process's memory by itself (CI 2026-11-22: 600 MB down to 0.2
		// GB), so no job's RAM covers the shortfall and nothing is asked to save. What must hold: a
		// frozen download is never killed outright.
		time.Sleep(10 * time.Second)
		if strings.Contains(e.log(), "download: killed") {
			t.Fatalf("a frozen download was killed:\n%s", e.log())
		}
	} else {
		eventually(t, "asked to save, not killed", 30*time.Second, func() bool { return strings.Contains(readFile(log), "checkpointed") })
	}
	e.setFake(map[string]any{"idle": 9999.0, "foreground": "", "net_mbps": 0.0}) // keep the real network out of it
	select {
	case <-done:
		if !strings.Contains(readFile(log), "finished") {
			t.Fatalf("download didn't finish:\n%s", readFile(log))
		}
	case <-time.After(240 * time.Second): // requeued: a 1 min back-off, then the 30 s measuring gap
		out, _ := e.ff("status")
		t.Fatalf("download never finished:\n%s", out)
	}
}
