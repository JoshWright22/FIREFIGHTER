package main

// End-to-end job scheduling, queue actions, and receipts.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- tests ----------------------------------------------------------------------------------------

func TestRunPropagatesExitAndReceipt(t *testing.T) {
	e := newEnv(t, nil)
	out, code := e.job("exit:3", "--minutes", "1", "--name", "exit three")
	if code != 3 {
		t.Fatalf("exit code %d, want 3\n%s", code, out)
	}
	if !strings.Contains(out, "helper-out") || !strings.Contains(out, "curtain call, failed (exit 3)") {
		t.Fatalf("missing output or receipt:\n%s", out)
	}
	out, code = e.job("exit:0", "--minutes", "1")
	if code != 0 || !strings.Contains(out, "curtain call, ok") {
		t.Fatalf("ok run: %d\n%s", code, out)
	}
}

func TestDetachAndWaitReturnReceipt(t *testing.T) {
	e := newEnv(t, nil)
	id := e.detach("exit:0", "--minutes", "1", "--name", "detached")
	out, code := e.ff("wait", id)
	if code != 0 {
		t.Fatalf("wait exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"outcome": "ok"`) || !strings.Contains(out, "helper-out") {
		t.Fatalf("receipt or log tail missing:\n%s", out)
	}
}

func TestCancelWaitingJob(t *testing.T) {
	e := newEnv(t, map[string]any{"idle": 0.0, "foreground": "game.exe", "gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	id := e.detach("exit:0", "--gpu", "--minutes", "1")
	time.Sleep(time.Second)
	if out, code := e.ff("cancel", id); code != 0 {
		t.Fatal(out)
	}
	out, _ := e.ff("wait", id, "--timeout", "20")
	if !strings.Contains(out, `"outcome": "cancelled"`) {
		t.Fatalf("cancel:\n%s", out)
	}
}

func TestShortCPUJobsRunConcurrently(t *testing.T) {
	e := newEnv(t, nil)
	log := filepath.Join(e.home, "cpu.txt")
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.job("sleep:2000:"+log, "--minutes", "1") }()
	}
	wg.Wait()
	if el := time.Since(start); el > 12*time.Second {
		t.Fatalf("CPU jobs should not take turns (took %s)", el)
	}
}

func TestMarketQuoteAndRebid(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	e.ff("config", "set", "gpu.max_jobs", "1")
	hold := e.detach("pausable:20:"+filepath.Join(e.home, "hold.txt"), "--gpu", "--minutes", "30")
	eventually(t, "holder running", 15*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		return len(r) == 1
	})
	out, code := e.ff("quote", "--gpu", "--minutes", "10", "--json")
	if code != 0 {
		t.Fatal(out)
	}
	var q struct {
		Busy map[string]float64
		Menu []struct {
			Tier     string
			StartsIn float64 `json:"starts_in_min"`
			Cost     float64
		}
		Do string
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &q); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(q.Menu) != 3 || q.Busy["15m"] == 0 || q.Do == "" || strings.Contains(q.Do, "urgent") {
		t.Fatalf("quote incomplete or escalating: %+v", q)
	}
	spot, normal, urgent := q.Menu[0], q.Menu[1], q.Menu[2]
	if urgent.StartsIn > normal.StartsIn || urgent.Cost <= normal.Cost || spot.Cost >= normal.Cost || spot.StartsIn < normal.StartsIn {
		t.Fatalf("tiers out of order: %+v", q.Menu)
	}
	id := e.detach("exit:0", "--gpu", "--minutes", "5", "--bid", "1")
	time.Sleep(time.Second)
	if out, code := e.ff("bid", id, "9"); code != 0 || !strings.Contains(out, "bid 9") {
		t.Fatalf("rebid: %d %s", code, out)
	}
	start := time.Now()
	w, code := e.ff("wait", id, "--timeout", "2")
	if code != 124 || time.Since(start) > 10*time.Second {
		t.Fatalf("wait --timeout must return on time with 124: %d after %s\n%s", code, time.Since(start), w)
	}
	if !strings.Contains(w, "waiting #1") || !strings.Contains(w, "gpu 5m: start") || !strings.Contains(w, "do: ") {
		t.Fatalf("a waiting job must carry its market line:\n%s", w)
	}
	e.ff("cancel", hold)
}

func TestDetachKeepsJobFlags(t *testing.T) {
	e := newEnv(t, nil)
	self, _ := os.Executable()
	out, err := e.cmd([]string{"FF_HELPER=exit:0"}, "run", "--detach", "--minutes", "1", "--", self, "--detach").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	w, code := e.ff("wait", id, "--timeout", "30")
	if code != 0 || !strings.Contains(w, `"outcome": "ok"`) {
		t.Fatalf("detached job with its own --detach flag: %d\n%s", code, w)
	}
}

func TestExit75Requeues(t *testing.T) {
	e := newEnv(t, nil)
	marker := filepath.Join(e.home, "once")
	self, _ := os.Executable()
	out, err := e.cmd([]string{"FF_HELPER=once75:" + marker}, "run", "--minutes", "1", "--", self).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "back in line") || !strings.Contains(string(out), "curtain call, ok") {
		t.Fatalf("exit 75 must requeue and the rerun must finish:\n%s", out)
	}
}

// Network-heavy jobs take turns (network.max_jobs 1), declared with --net or detected from the
// command (here a curl call is detected even without --net).
func TestNetworkJobsTakeTurns(t *testing.T) {
	e := newEnv(t, map[string]any{"net_mbps": 0.0})
	log := filepath.Join(e.home, "net.txt")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.job(fmt.Sprintf("sleep:1500:%s", log), "--net", "--minutes", "1")
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
			t.Fatalf("network jobs overlapped with network.max_jobs=1:\n%s", readFile(log))
		}
	}
	if !netCommand([]string{"curl", "-L", "-o", "x", "https://example.com"}) || !netCommand([]string{"docker", "pull", "img"}) ||
		!netCommand([]string{"cmd", "/c", "pip install torch"}) || netCommand([]string{"python", "train.py"}) ||
		!netCommand([]string{"sh", "-c", "cd x && pip install -r r.txt"}) || !netCommand([]string{"pip", "--index-url", "U", "install", "x"}) ||
		!netCommand([]string{"git", "-C", "d", "clone", "u"}) || !netCommand([]string{"docker", "compose", "pull"}) ||
		netCommand([]string{"docker", "build", "."}) || netCommand([]string{"cargo", "install", "ripgrep"}) {
		t.Fatal("network command detection")
	}
}

// 11/20: an agent cancelled and resubmitted its job 8 times, changing --vram 14 -> 12 -> 10 -> 9 and
// missing each time by a little. The wait line says what would start it, and `amend` changes the
// waiting job in place: same id, same waiting client, and it then runs.
func TestAmendWaitingJob(t *testing.T) {
	// 16 GB card with 6.5 GB in use: 9.5 free, less the human's reserve.
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16, "used_gb": 6.5}}})
	id := e.detach("exit:0", "--gpu", "--minutes", "1", "--vram", "9")
	time.Sleep(2 * time.Second)
	out, _ := e.ff("wait", id, "--timeout", "1")
	if !strings.Contains(out, "would start now with --vram") || !strings.Contains(out, "firefighter amend "+id) {
		t.Fatalf("the wait line should say what would start it:\n%s", out)
	}
	if out, code := e.ff("amend", id, "--vram", "7", "--minutes", "2"); code != 0 || !strings.Contains(out, "amended") {
		t.Fatalf("amend: %d\n%s", code, out)
	}
	if out, code := e.ff("wait", id, "--timeout", "30"); code != 0 || !strings.Contains(out, `"outcome": "ok"`) {
		t.Fatalf("the amended job should run under its original id: %d\n%s", code, out)
	}
	if !strings.Contains(e.log(), "amended (vram 9.0 -> 7.0 GB, minutes 1 -> 2)") {
		t.Fatalf("log:\n%s", e.log())
	}
	if out, code := e.ff("amend", id, "--ram", "2"); code == 0 || !strings.Contains(out, "only changes a waiting job") {
		t.Fatalf("amending a finished job must fail: %d\n%s", code, out)
	}
}

// OpenPAI-style exit classification: a job that fails on its own gets a reason, whether a retry
// can help, and a fix; out of VRAM, its learned need goes up for the next run.
func TestFailedJobSaysWhyAndLearns(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16, "used_gb": 1}}})
	out, code := e.job("cudaoom", "--gpu", "--minutes", "1", "--vram", "4", "--name", "oomer")
	if code != 1 || !strings.Contains(out, "failed because the GPU ran out of memory") || !strings.Contains(out, "do: declare more VRAM") {
		t.Fatalf("receipt should say why it failed: %d\n%s", code, out)
	}
	if !strings.Contains(e.log(), "oomer: failed: the GPU ran out of memory (vram)") {
		t.Fatalf("log:\n%s", e.log())
	}
}

// Idempotent submit (after Gczmy/sched): the same request id in the same project never queues a
// second copy; a retry gets the first job.
func TestRequestIDNeverQueuesTwice(t *testing.T) {
	e := newEnv(t, nil)
	self, _ := os.Executable()
	sub := func() string {
		out, err := e.cmd([]string{"FF_HELPER=sleep:3000:" + filepath.Join(e.home, "s.txt")}, "run", "--detach", "--request-id", "eval-42", "--minutes", "1", "--project", "demo", "--", self).CombinedOutput()
		if err != nil {
			t.Fatalf("submit: %v\n%s", err, out)
		}
		return string(out)
	}
	first := sub()
	id := strings.TrimSpace(strings.Split(first, "\n")[0])
	second := sub()
	if strings.TrimSpace(strings.Split(second, "\n")[0]) != id || !strings.Contains(second, "already submitted") {
		t.Fatalf("a retried submit should get the first job %s:\n%s", id, second)
	}
	st := e.status()
	r, _ := st["running"].([]any)
	w, _ := st["waiting"].([]any)
	if len(r)+len(w) != 1 {
		t.Fatalf("want one job, got %d running %d waiting", len(r), len(w))
	}
	e.ff("wait", id, "--timeout", "30") // let it finish: Windows can't delete a log a job still writes
}

// Review round 4 (Fable): a job's leftover process holding the output pipe must not keep the job
// open (the output is teed whenever it isn't a terminal: agents, detached jobs).
func TestLeftoverProcessDoesNotHoldTheJob(t *testing.T) {
	e := newEnv(t, nil)
	start := time.Now()
	out, code := e.job("leaveholder", "--minutes", "1", "--name", "builder")
	if code != 0 || !strings.Contains(out, "curtain call, ok") {
		t.Fatalf("receipt: %d\n%s", code, out)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("the job stayed open %v while its leftover held the output", d)
	}
}
