package main

// End-to-end daemon lifecycle, setup, and status reporting.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/daemon"
)

// What a run used is learned and shown: `firefighter forecast` lists the command with what its next
// run is expected to need, and the human's reserve per state.
func TestForecastShowsWhatWasLearned(t *testing.T) {
	e := newEnv(t, nil)
	e.extra = []string{"FF_HOG_MB=300"}
	if out, code := e.job("sleep:2500:"+filepath.Join(e.home, "s.txt"), "--minutes", "1", "--name", "hog job"); code != 0 {
		t.Fatalf("job failed:\n%s", out)
	}
	out, _ := e.ff("forecast")
	if !strings.Contains(out, "the human, by state") || !strings.Contains(out, "hog job") {
		t.Fatalf("forecast doesn't list the human and the command:\n%s", out)
	}
	var f daemon.Forecast
	js, _ := e.ff("forecast", "--json")
	if err := json.Unmarshal([]byte(js[strings.Index(js, "{"):]), &f); err != nil {
		t.Fatalf("forecast --json: %v\n%s", err, js)
	}
	if len(f.Commands) != 1 || f.Commands[0].RAMGB < 0.3 || f.Commands[0].RAMGB > 0.6 || len(f.Human) != 4 {
		t.Fatalf("the 300 MB job should be expected to need 0.3-0.6 GB next time: %+v", f)
	}
}

func TestConfigChangesAreLive(t *testing.T) {
	e := newEnv(t, nil)
	e.status()
	e.ff("config", "set", "preset", "fair")
	eventually(t, "policy reload", 10*time.Second, func() bool {
		p, _ := e.status()["policy"].(map[string]any)
		return p["order"] == "drf" && p["pricing"] == "free"
	})
	if out, code := e.ff("config", "set", "order.policy", "bogus"); code == 0 {
		t.Fatalf("bad policy accepted:\n%s", out)
	}
}

func TestDaemonRestartKeepsGoverningRunningJob(t *testing.T) {
	e := newEnv(t, nil)
	log := filepath.Join(e.home, "restart.txt")
	id := e.detach("pausable:25:"+log, "--minutes", "1")
	eventually(t, "running", 15*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		return len(r) == 1
	})
	e.ff("daemon", "--stop")
	time.Sleep(time.Second)
	eventually(t, "re-registered after restart", 30*time.Second, func() bool {
		r, _ := e.status()["running"].([]any) // status restarts the daemon
		return len(r) == 1
	})
	if out, code := e.ff("wait", id, "--timeout", "60"); code != 0 && !strings.Contains(out, "finished") {
		if !strings.Contains(readFile(log), "finished") {
			t.Fatalf("job lost across daemon restart: %d\n%s\n%s", code, out, readFile(log))
		}
	}
}

func TestPlansShowInStatus(t *testing.T) {
	e := newEnv(t, nil)
	e.ff("plan", "big eval sweep", "--when", "tonight", "--minutes", "90", "--gpu", "80", "--project", "demo")
	p, _ := e.status()["plans"].([]any)
	if len(p) != 1 {
		t.Fatalf("plan missing: %v", p)
	}
	e.ff("plan", "--done", "--project", "demo")
	p, _ = e.status()["plans"].([]any)
	if len(p) != 0 {
		t.Fatalf("plan not removed: %v", p)
	}
	if !strings.Contains(readFile(filepath.Join(e.home, "STATUS.md")), "firefighter status") {
		t.Fatal("STATUS.md not written")
	}
}

func TestSetupTeachesAgentsAndUninstallCleans(t *testing.T) {
	e := newEnv(t, nil)
	h := t.TempDir()
	for _, d := range []string{".claude", ".codex", ".gemini"} {
		_ = os.MkdirAll(filepath.Join(h, d), 0o755)
	}
	_ = os.WriteFile(filepath.Join(h, ".claude", "CLAUDE.md"), []byte("# mine\n"), 0o644)
	extra := []string{"HOME=" + h, "USERPROFILE=" + h, "CLAUDE_CONFIG_DIR=", "CODEX_HOME=",
		"APPDATA=" + filepath.Join(h, "AppData"), "XDG_CONFIG_HOME=" + filepath.Join(h, ".config"),
		"T3CODE_HOME=" + filepath.Join(h, ".t3", "userdata")}
	if out, err := e.cmd(extra, "setup").CombinedOutput(); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, f := range []string{".claude/CLAUDE.md", ".codex/AGENTS.md", ".gemini/GEMINI.md"} {
		if !strings.Contains(readFile(filepath.Join(h, f)), "firefighter run") {
			t.Fatalf("%s not taught", f)
		}
	}
	if !strings.Contains(readFile(filepath.Join(h, ".codex", "config.toml")), "mcp_servers.firefighter") {
		t.Fatal("codex MCP not registered")
	}
	if out, err := e.cmd(extra, "uninstall").CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if c := readFile(filepath.Join(h, ".claude", "CLAUDE.md")); strings.Contains(c, "firefighter") || !strings.Contains(c, "# mine") {
		t.Fatalf("uninstall left rules or ate user text:\n%s", c)
	}
}

func TestOnlyOneDaemon(t *testing.T) {
	e := newEnv(t, nil)
	before := e.status()["time"]
	out, code := e.ff("daemon") // returns at once (exit 0, so service managers don't crash-loop)
	if code != 0 || !strings.Contains(out, "already running") {
		t.Fatalf("a second daemon must step aside quietly: %d\n%s", code, out)
	}
	if e.status()["time"] == before {
		t.Fatal("the first daemon must still be serving")
	}
}

func TestStatuslineAndEvents(t *testing.T) {
	e := newEnv(t, nil)
	if out, _ := e.ff("statusline"); !strings.Contains(out, "off") {
		t.Fatalf("statusline must not start the daemon and must say off:\n%s", out)
	}
	e.status()
	out, code := e.ff("statusline", "--json")
	if code != 0 || !strings.Contains(out, `"text":"◆`) || !strings.Contains(out, `"tooltip"`) {
		t.Fatalf("statusline --json: %d\n%s", code, out)
	}
	c := e.cmd(nil, "events")
	pipe, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil || !strings.Contains(line, `"type":"status"`) {
		t.Fatalf("events: %v %q", err, line)
	}
}
