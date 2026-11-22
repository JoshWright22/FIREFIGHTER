package main

// The 2026-11-22 fixes from the nanoTSFM sprint thread.

import (
	"os"
	"os/exec"
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
