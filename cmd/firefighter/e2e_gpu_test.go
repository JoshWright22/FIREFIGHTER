package main

// End-to-end GPU admission, placement, and game preemption.

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGPUTurnsDoNotOverlap(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16, "used_gb": 0, "util": 0}}})
	if out, code := e.ff("config", "set", "gpu.max_jobs", "1"); code != 0 {
		t.Fatal(out)
	}
	log := filepath.Join(e.home, "turns.txt")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.job(fmt.Sprintf("sleep:1500:%s", log), "--gpu", "--minutes", "1", "--vram", "4")
		}()
	}
	wg.Wait()
	var spans [][2]int64
	for _, line := range strings.Split(strings.TrimSpace(readFile(log)), "\n") {
		f := strings.Fields(line)
		n, _ := strconv.ParseInt(f[1], 10, 64)
		if f[0] == "start" {
			spans = append(spans, [2]int64{n, 0})
		} else {
			spans[len(spans)-1][1] = n
		}
	}
	if len(spans) != 3 {
		t.Fatalf("want 3 runs, log:\n%s", readFile(log))
	}
	for i := 1; i < len(spans); i++ {
		if spans[i][0] < spans[i-1][1] {
			t.Fatalf("GPU jobs overlapped with max_jobs=1:\n%s", readFile(log))
		}
	}
}

func TestVRAMAdmission(t *testing.T) {
	// 16 GB card, 13 GB used by something else: a job needing 4 GB must wait; freeing VRAM lets it run.
	gpu := func(used float64) map[string]any {
		return map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16, "used_gb": used}}}
	}
	e := newEnv(t, gpu(13))
	id := e.detach("exit:0", "--gpu", "--minutes", "1", "--vram", "4")
	time.Sleep(2 * time.Second)
	out, _ := e.ff("wait", id, "--timeout", "1")
	if !strings.Contains(out, "VRAM") {
		t.Fatalf("job should wait on VRAM:\n%s", out)
	}
	e.setFake(gpu(2))
	if out, code := e.ff("wait", id, "--timeout", "30"); code != 0 {
		t.Fatalf("job should run once VRAM frees: %d\n%s", code, out)
	}
}

func TestGamePausesGPUJobAndResumes(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	e.ff("config", "set", "reserve.game_cool_seconds", "1")
	log := filepath.Join(e.home, "pausable.txt")
	id := e.detach("pausable:30:"+log, "--gpu", "--minutes", "1")
	eventually(t, "job running", 15*time.Second, func() bool { return exists(log) || strings.Contains(e.mustWaitState(id), "running") })
	e.setFake(map[string]any{"idle": 0.0, "foreground": "game.exe", "gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	eventually(t, "cooperative pause", 15*time.Second, func() bool { return strings.Contains(readFile(log), "paused") })
	st := e.status()
	if st["user"] != "game" {
		t.Fatalf("user state %v, want game", st["user"])
	}
	e.setFake(map[string]any{"idle": 9999.0, "foreground": "", "gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	// Stepping down from game needs two readings 10 s apart (hysteresis), then the job resumes.
	eventually(t, "resume", 30*time.Second, func() bool { return strings.Contains(readFile(log), "resumed") })
	if out, code := e.ff("wait", id, "--timeout", "60"); code != 0 {
		t.Fatalf("finish after resume: %d\n%s", code, out)
	}
	if !strings.Contains(readFile(log), "finished") {
		t.Fatalf("job didn't finish:\n%s", readFile(log))
	}
}

// Two cards: the first nearly full, the second free. A GPU job is admitted on the free one and
// pinned there in its vendor's numbering, with CUDA ordered like nvidia-smi.
func TestMultiGPUPlacesJobOnTheFreeCard(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{
		{"index": 0, "name": "Busy", "vendor": "nvidia", "vram_gb": 16, "used_gb": 15.5, "util": 0},
		{"index": 1, "name": "Free", "vendor": "nvidia", "vram_gb": 16, "used_gb": 0, "util": 0},
	}})
	out := filepath.Join(e.home, "env.txt")
	if o, code := e.job("env:CUDA_VISIBLE_DEVICES,CUDA_DEVICE_ORDER,HIP_VISIBLE_DEVICES:"+out, "--gpu", "--minutes", "1", "--vram", "4"); code != 0 {
		t.Fatalf("job failed %d:\n%s", code, o)
	}
	got := readFile(out)
	if !strings.Contains(got, "CUDA_VISIBLE_DEVICES=1") || !strings.Contains(got, "CUDA_DEVICE_ORDER=PCI_BUS_ID") || !strings.Contains(got, "HIP_VISIBLE_DEVICES=\n") {
		t.Fatalf("job should be pinned to NVIDIA card 1:\n%s", got)
	}
}

// NVIDIA card full, AMD card free: a ROCm job (--gpu-vendor amd) goes to the AMD card, pinned with
// ROCm's variables in AMD's own numbering (device 0), and CUDA isn't touched. Without the flag a
// GPU job waits for the NVIDIA card (most GPU code is CUDA).
func TestMixedVendorsPinWithTheirOwnVariables(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{
		{"index": 0, "name": "GeForce", "vendor": "nvidia", "vram_gb": 12, "used_gb": 11.8, "util": 0},
		{"index": 0, "name": "Radeon", "vendor": "amd", "vram_gb": 24, "used_gb": 0, "util": 0},
	}})
	out := filepath.Join(e.home, "env.txt")
	if o, code := e.job("env:HIP_VISIBLE_DEVICES,ROCR_VISIBLE_DEVICES,CUDA_VISIBLE_DEVICES:"+out, "--gpu", "--gpu-vendor", "amd", "--minutes", "1", "--vram", "8"); code != 0 {
		t.Fatalf("job failed %d:\n%s", code, o)
	}
	got := readFile(out)
	if !strings.Contains(got, "HIP_VISIBLE_DEVICES=0") || !strings.Contains(got, "ROCR_VISIBLE_DEVICES=0") || !strings.Contains(got, "CUDA_VISIBLE_DEVICES=\n") {
		t.Fatalf("job should be pinned to the AMD card with ROCm variables:\n%s", got)
	}
}

// A declared need waits for a card that can hold it rather than starting, shrunk, on a smaller
// idle one: 24 GB busy + 12 GB idle, --vram 20 waits for the 24 GB card.
func TestDeclaredVRAMWaitsForABigEnoughCard(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{
		{"index": 0, "name": "Big", "vendor": "nvidia", "vram_gb": 24, "used_gb": 20, "util": 0},
		{"index": 1, "name": "Small", "vendor": "nvidia", "vram_gb": 12, "used_gb": 0, "util": 0},
	}})
	id := e.detach("exit:0", "--gpu", "--minutes", "1", "--vram", "20")
	time.Sleep(3 * time.Second)
	if st := e.mustWaitState(id); !strings.Contains(st, "waiting") {
		t.Fatalf("a 20 GB job must not start on the 12 GB card: %s", st)
	}
	out, code := e.ff("run", "--gpu", "--minutes", "1", "--vram", "40", "--", "x")
	if code == 0 || !strings.Contains(out, "no GPU here has that much") {
		t.Fatalf("--vram larger than every card must be refused: %d %s", code, out)
	}
}

// Each card takes turns on its own: a card at 95% load doesn't hold back a job that fits on an
// idle one, and the job goes to the idle card even when the busy one has more VRAM free.
func TestBusyCardDoesNotBlockTheIdleOne(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{
		{"index": 0, "name": "Busy", "vendor": "nvidia", "vram_gb": 24, "used_gb": 2, "util": 95},
		{"index": 1, "name": "Idle", "vendor": "nvidia", "vram_gb": 24, "used_gb": 6, "util": 0},
	}})
	out := filepath.Join(e.home, "env.txt")
	if o, code := e.job("env:CUDA_VISIBLE_DEVICES:"+out, "--gpu", "--minutes", "1", "--vram", "4"); code != 0 {
		t.Fatalf("job failed %d:\n%s", code, o)
	}
	if got := readFile(out); !strings.Contains(got, "CUDA_VISIBLE_DEVICES=1") {
		t.Fatalf("job should run on the idle card 1:\n%s", got)
	}
}
