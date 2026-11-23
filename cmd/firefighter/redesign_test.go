package main

// The 2026-11-22 fixes from the nanoTSFM sprint thread.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A waiting `firefighter run` whose launcher died (a killed Python runner) drops its job instead
// of leaving it queued for no one.
func TestOrphanedRunLeavesTheLine(t *testing.T) {
	e := newEnv(t, map[string]any{"idle": 0.0, "foreground": "game.exe", "gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	self, _ := os.Executable()
	args := strings.Join([]string{"run", "--gpu", "--minutes", "1", "--name", "orphan", "--", self}, "\x1f")
	// The launcher is started directly, not as a job: a job's leftovers are killed when it ends,
	// and the orphan would go with them before it could drop its own job.
	launcher := exec.Command(self)
	launcher.Env = append(append([]string{}, e.vars...), "FF_HELPER=launch", "FF_BIN="+binPath, "FF_LAUNCH_ARGS="+args)
	if out, err := launcher.CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v\n%s", err, out)
	}
	eventually(t, "the orphaned run's job is dropped", 30*time.Second, func() bool {
		return strings.Contains(e.log(), "orphan: cancelled")
	})
	if !strings.Contains(e.log(), "orphan [") {
		t.Fatalf("the orphan never queued:\n%s", e.log())
	}
}

// cancel says what it did; show prints one job; `<command> --help` prints that command's help.
func TestCancelShowAndHelp(t *testing.T) {
	e := newEnv(t, map[string]any{"idle": 0.0, "foreground": "game.exe", "gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}})
	id := e.detach("exit:0", "--gpu", "--minutes", "1", "--name", "held", "--ram", "2")
	time.Sleep(time.Second)
	out, code := e.ff("show", id)
	if code != 0 || !strings.Contains(out, id+" held") || !strings.Contains(out, "declared:") || !strings.Contains(out, "counted:") || !strings.Contains(out, "#1 in the GPU line") {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if out, code := e.ff("cancel", id); code != 0 || !strings.Contains(out, id+" held: cancelled") {
		t.Fatalf("cancel must confirm: %d\n%s", code, out)
	}
	if out, code := e.ff("setup", "--help"); code != 0 || !strings.Contains(out, "firefighter setup") || strings.Contains(out, "takes no arguments") {
		t.Fatalf("setup --help: %d\n%s", code, out)
	}
}

func TestHashCmdTellsConfigsApart(t *testing.T) {
	a := hashCmd([]string{"python.exe", "lab/fresh_train.py", "--config", "configs/transfer-data-pow10.yaml", "--seed", "7"})
	b := hashCmd([]string{"python.exe", "lab/fresh_train.py", "--config", "configs/ctl-port.yaml", "--seed", "2"})
	c := hashCmd([]string{"python.exe", "lab/fresh_train.py", "--config=other/transfer-data-pow10.yaml", "--seed", "1"})
	if a == b || a != c {
		t.Fatalf("configs must be told apart, seeds and folders not: %s %s %s", a, b, c)
	}
	if hashCmd([]string{"python", "train.py", "--lr", "3"}) != hashCmd([]string{"python", "./train.py"}) {
		t.Fatal("without a config, arguments still don't split a history")
	}
}

func TestAgentConfigRuleSaysWhy(t *testing.T) {
	if err := agentConfigRule("gpu.pack_below_util", false); err == nil || !strings.Contains(err.Error(), "guard.agents_can_configure is off") || strings.Contains(err.Error(), "agents may change") {
		t.Fatalf("off: %v", err)
	}
	if err := agentConfigRule("gpu.pack_below_util", true); err != nil {
		t.Fatalf("on: gpu.* is allowed: %v", err)
	}
	if err := agentConfigRule("reserve.min_free_gb", true); err == nil || !strings.Contains(err.Error(), "agents may change order.*, pricing.* and gpu.* only") {
		t.Fatalf("on, reserve.*: %v", err)
	}
}

// UnomiBackend 11/23: `firefighter wait` exited with "connection refused" while the guard
// restarted, and a script took that as the job's end. wait reconnects and keeps waiting: it
// returns the job's real receipt.
func TestWaitSurvivesAGuardRestart(t *testing.T) {
	gpus := []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16}}
	e := newEnv(t, map[string]any{"idle": 0.0, "foreground": "game.exe", "gpus": gpus})
	id := e.detach("exit:0", "--gpu", "--minutes", "1", "--name", "survivor")
	time.Sleep(time.Second)
	done := make(chan string, 1)
	go func() {
		out, _ := e.cmd(nil, "wait", id, "--timeout", "120").CombinedOutput()
		done <- string(out)
	}()
	time.Sleep(2 * time.Second)
	if out, code := e.ff("daemon", "--stop"); code != 0 {
		t.Fatalf("stop: %s", out)
	}
	time.Sleep(2 * time.Second)
	e.setFake(map[string]any{"idle": 9999.0, "foreground": "", "gpus": gpus}) // the game ends: it may run
	select {
	case out := <-done:
		if !strings.Contains(out, `"outcome": "ok"`) {
			t.Fatalf("wait didn't follow the job through the restart:\n%s\n%s", out, e.log())
		}
	case <-time.After(150 * time.Second):
		t.Fatalf("wait never returned:\n%s", e.log())
	}
}

// Bug 81417522 (11/23): the guard restarted while a job ran; when the job was later sent back in
// line (evicted, or checkpointed with exit 75), its client re-queued it to the old guard's address,
// got "connection refused" and exited, so the job ended instead of running again. The client now
// re-queues with whichever guard is running.
func TestRequeueAfterAGuardRestart(t *testing.T) {
	e := newEnv(t, nil)
	marker := filepath.Join(e.home, "first-run-done")
	id := e.detach("once75slow:8000:"+marker, "--minutes", "1", "--name", "restarted under it")
	eventually(t, "first run started", 20*time.Second, func() bool { return exists(marker) })
	if out, code := e.ff("daemon", "--stop"); code != 0 {
		t.Fatalf("stop: %s", out)
	}
	if out, code := e.ff("status"); code != 0 { // a new guard, on a new port
		t.Fatalf("status: %s", out)
	}
	out, code := e.ff("wait", id, "--timeout", "90")
	if code != 0 || !strings.Contains(out, `"outcome": "ok"`) || !strings.Contains(out, "second run") {
		t.Fatalf("the job didn't run again after its guard restarted: %d\n%s\n%s", code, out, e.log())
	}
}

// fdf58d99: a GPU job is told its VRAM share (the card less the human's reserve less what else is
// on it), so it can cap its framework's cache instead of being stopped for growing into the reserve.
func TestGPUJobIsToldItsVRAMShare(t *testing.T) {
	e := newEnv(t, map[string]any{"gpus": []map[string]any{{"index": 0, "name": "Fake", "vram_gb": 16, "used_gb": 2}}})
	file := filepath.Join(e.home, "env.txt")
	if out, code := e.job("env:FIREFIGHTER_VRAM_GB,FIREFIGHTER_VRAM_FRACTION:"+file, "--gpu", "--minutes", "1", "--vram", "4"); code != 0 {
		t.Fatalf("job: %d\n%s", code, out)
	}
	got := readFile(file)
	if !strings.Contains(got, "FIREFIGHTER_VRAM_GB=1") || !strings.Contains(got, "FIREFIGHTER_VRAM_FRACTION=0.") {
		t.Fatalf("the job's environment lacks its VRAM share:\n%s", got)
	}
}

// 5658b8b4: a stopped job waited out its back-off with no way to try again; cancel and resubmit was
// the only way out. `firefighter retry` ends the wait now: a job that checkpointed (exit 75, a
// one-minute back-off) runs again within seconds.
func TestRetryEndsAStoppedJobsBackOff(t *testing.T) {
	e := newEnv(t, nil)
	marker := filepath.Join(e.home, "first-run-done")
	id := e.detach("once75:"+marker, "--minutes", "1", "--name", "retried")
	eventually(t, "stopped and backing off", 30*time.Second, func() bool { return strings.Contains(e.mustWaitState(id), "tries again in") })
	start := time.Now()
	if out, code := e.ff("retry", id); code != 0 || !strings.Contains(out, "back-off ended") {
		t.Fatalf("retry: %d\n%s", code, out)
	}
	out, code := e.ff("wait", id, "--timeout", "40")
	if code != 0 || !strings.Contains(out, "second run") || time.Since(start) > 30*time.Second {
		t.Fatalf("the retried job didn't run again at once (%s): %d\n%s", time.Since(start).Round(time.Second), code, out)
	}
	if out, code := e.ff("retry", id); code == 0 || !strings.Contains(out, "is done") {
		t.Fatalf("retrying a finished job must be refused: %d\n%s", code, out)
	}
}
