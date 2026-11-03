package main

// End-to-end tests: build the real binary, start a real daemon in a temp home, and script the
// human, memory pressure and a GPU through FIREFIGHTER_FAKE_FILE. The test binary doubles as the
// job payload (FF_HELPER). CI runs these on Windows, macOS and Linux VMs.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

var binPath string

func TestMain(m *testing.M) {
	if h := os.Getenv("FF_HELPER"); h != "" {
		os.Exit(helper(h))
	}
	dir, err := os.MkdirTemp("", "ff-bin")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "firefighter")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// helper is the job payload. Modes: exit:N | sleep:MS:FILE | pausable:SECONDS:FILE
func helper(spec string) int {
	f := strings.Split(spec, ":")
	switch f[0] {
	case "exit":
		fmt.Println("helper-out")
		n, _ := strconv.Atoi(f[1])
		return n
	case "sleep":
		ms, _ := strconv.Atoi(f[1])
		file := strings.Join(f[2:], ":")
		appendLine(file, fmt.Sprintf("start %d", time.Now().UnixNano()))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		appendLine(file, fmt.Sprintf("end %d", time.Now().UnixNano()))
		return 0
	case "env": // env:NAME,NAME2:file writes NAME=value for each (empty if unset)
		file := strings.Join(f[2:], ":")
		for _, k := range strings.Split(f[1], ",") {
			appendLine(file, k+"="+os.Getenv(k))
		}
		return 0
	case "once75": // checkpoint-and-exit the first time, succeed the second
		marker := strings.Join(f[1:], ":")
		if !exists(marker) {
			_ = os.WriteFile(marker, nil, 0o644)
			return 75
		}
		fmt.Println("second run")
		return 0
	case "burn": // spin CPU for N seconds
		secs, _ := strconv.Atoi(f[1])
		end := time.Now().Add(time.Duration(secs) * time.Second)
		x := 0
		for time.Now().Before(end) {
			x++
		}
		return x & 0
	case "pausable":
		secs, _ := strconv.Atoi(f[1])
		file := strings.Join(f[2:], ":")
		for i := 0; i < secs*10; i++ {
			if p := os.Getenv("FIREFIGHTER_PAUSE_FILE"); p != "" && exists(p) {
				ack := os.Getenv("FIREFIGHTER_PAUSED_FILE")
				_ = os.WriteFile(ack, nil, 0o644)
				appendLine(file, "paused")
				for exists(p) {
					time.Sleep(100 * time.Millisecond)
				}
				_ = os.Remove(ack)
				appendLine(file, "resumed")
			}
			if c := os.Getenv("FIREFIGHTER_CHECKPOINT_FILE"); c != "" && exists(c) {
				appendLine(file, "checkpointed")
				return 75
			}
			time.Sleep(100 * time.Millisecond)
		}
		appendLine(file, "finished")
		return 0
	}
	return 99
}

func appendLine(file, line string) {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

type env struct {
	t    *testing.T
	home string
	fake string
	vars []string
}

func newEnv(t *testing.T, fake map[string]any) *env {
	t.Helper()
	home := t.TempDir()
	e := &env{t: t, home: home, fake: filepath.Join(home, "fake.json")}
	if fake == nil {
		fake = map[string]any{}
	}
	if _, ok := fake["idle"]; !ok {
		fake["idle"] = 9999.0 // away unless the test says otherwise
	}
	if _, ok := fake["foreground"]; !ok {
		fake["foreground"] = ""
	}
	e.setFake(fake)
	e.vars = append(os.Environ(), "FIREFIGHTER_HOME="+home, "FIREFIGHTER_FAKE_FILE="+e.fake, "NO_COLOR=1",
		"FIREFIGHTER_NO_AUTOSTART=1", "FIREFIGHTER_NO_CLI_REGISTER=1")
	t.Cleanup(e.cleanup)
	return e
}

// cleanup cancels whatever is still queued or running, then stops the daemon.
func (e *env) cleanup() {
	if e.t.Failed() {
		e.t.Logf("daemon.log: %s", readFile(filepath.Join(e.home, "daemon.log")))
	}
	if out, err := e.cmd(nil, "status", "--json").Output(); err == nil {
		var st struct {
			Running, Waiting []struct{ ID string }
		}
		if i := strings.Index(string(out), "{"); i >= 0 && json.Unmarshal(out[i:], &st) == nil {
			for _, j := range append(st.Running, st.Waiting...) {
				_ = e.cmd(nil, "cancel", j.ID).Run()
			}
		}
	}
	time.Sleep(500 * time.Millisecond)
	_ = e.cmd(nil, "daemon", "--stop").Run()
	time.Sleep(300 * time.Millisecond)
}

func (e *env) setFake(m map[string]any) {
	// Every update keeps the scripted human: a real fullscreen app on the test machine must never leak in.
	if _, ok := m["idle"]; !ok {
		m["idle"] = 9999.0
	}
	if _, ok := m["foreground"]; !ok {
		m["foreground"] = ""
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(e.fake, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) cmd(extra []string, args ...string) *exec.Cmd {
	c := exec.Command(binPath, args...)
	c.Env = append(append([]string{}, e.vars...), extra...)
	return c
}

func (e *env) ff(args ...string) (string, int) {
	out, err := e.cmd(nil, args...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("%v: %v", args, err)
	}
	return string(out), code
}

func (e *env) job(helperSpec string, args ...string) (string, int) {
	self, _ := os.Executable()
	full := append(append([]string{"run"}, args...), "--", self)
	out, err := e.cmd([]string{"FF_HELPER=" + helperSpec}, full...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *env) detach(helperSpec string, args ...string) string {
	self, _ := os.Executable()
	full := append(append([]string{"run", "--detach"}, args...), "--", self)
	out, err := e.cmd([]string{"FF_HELPER=" + helperSpec}, full...).Output()
	if err != nil {
		e.t.Fatalf("detach: %v", err)
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0])
}

func (e *env) status() map[string]any {
	out, code := e.ff("status", "--json")
	if code != 0 {
		e.t.Fatalf("status: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &m); err != nil {
		e.t.Fatalf("status json: %v\n%s", err, out)
	}
	return m
}

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

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
	eventually(t, "resume", 15*time.Second, func() bool { return strings.Contains(readFile(log), "resumed") })
	if out, code := e.ff("wait", id, "--timeout", "60"); code != 0 {
		t.Fatalf("finish after resume: %d\n%s", code, out)
	}
	if !strings.Contains(readFile(log), "finished") {
		t.Fatalf("job didn't finish:\n%s", readFile(log))
	}
}

func (e *env) mustWaitState(id string) string {
	out, _ := e.ff("wait", id, "--timeout", "1")
	return out
}

func TestRAMPressureCheckpointsAndRequeues(t *testing.T) {
	v, _ := mem.VirtualMemory()
	total := float64(v.Total) / (1 << 30)
	e := newEnv(t, nil)
	e.ff("config", "set", "ladder.pressure_seconds", "1")
	e.ff("config", "set", "ladder.kill_below_ram_pct", "0.01")
	log := filepath.Join(e.home, "evict.txt")
	done := make(chan string, 1)
	go func() {
		out, _ := e.job("pausable:4:"+log, "--minutes", "1", "--name", "evictee")
		done <- out
	}()
	eventually(t, "job running", 15*time.Second, func() bool {
		st := e.status()
		r, _ := st["running"].([]any)
		return len(r) == 1
	})
	e.setFake(map[string]any{"idle": 9999.0, "foreground": "", "ram_avail_gb": total * 0.02}) // under the 5% reserve
	eventually(t, "checkpoint", 20*time.Second, func() bool { return strings.Contains(readFile(log), "checkpointed") })
	time.Sleep(1500 * time.Millisecond)
	e.setFake(map[string]any{"idle": 9999.0, "foreground": ""})
	select {
	case out := <-done:
		if !strings.Contains(out, "back in line") {
			t.Fatalf("evicted job should requeue:\n%s", out)
		}
		if !strings.Contains(readFile(log), "finished") {
			t.Fatalf("requeued job didn't finish:\n%s", readFile(log))
		}
	case <-time.After(90 * time.Second):
		t.Fatal("evicted job never finished")
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

func TestMCPServer(t *testing.T) {
	e := newEnv(t, nil)
	c := e.cmd(nil, "mcp")
	in, _ := c.StdinPipe()
	outPipe, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	r := bufio.NewReader(outPipe)
	call := func(id int, method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		in.Write(append(b, '\n'))
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m
	}
	init := call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "firefighter" {
		t.Fatalf("initialize: %v", init)
	}
	in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	list := call(2, "tools/list", map[string]any{})
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 5 {
		t.Fatalf("want 5 tools (config hidden from agents by default), got %d", n)
	}
	st := call(3, "tools/call", map[string]any{"name": "firefighter_status", "arguments": map[string]any{}})
	if st["result"].(map[string]any)["isError"] == true {
		t.Fatalf("status tool: %v", st)
	}
	cwd, _ := os.Getwd()
	cmdline := "echo mcp-job"
	sub := call(4, "tools/call", map[string]any{"name": "firefighter_submit", "arguments": map[string]any{"command": cmdline, "cwd": cwd, "minutes": 1}})
	text := sub["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	fields := strings.Fields(text)
	if len(fields) < 2 || (fields[0] != "queued" && fields[0] != "started") || len(text) > 400 {
		t.Fatalf("submit should be one short line: %s", text)
	}
	id := fields[1]
	var waitText string
	for i := 0; i < 4 && !strings.Contains(waitText, " done "); i++ {
		w := call(5+i, "tools/call", map[string]any{"name": "firefighter_wait", "arguments": map[string]any{"id": id, "timeout_s": 20}})
		waitText = w["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !strings.Contains(waitText, "done ok exit 0") || !strings.Contains(waitText, "mcp-job") {
		t.Fatalf("wait:\n%s", waitText)
	}
	stat := call(19, "tools/call", map[string]any{"name": "firefighter_status", "arguments": map[string]any{"minutes": 20, "gpu": true}})
	stText := stat["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(stText, "human:") || !strings.Contains(stText, "quote: gpu 20m") || !strings.Contains(stText, "do: ") || len(stText) > 1500 {
		t.Fatalf("status:\n%s", stText)
	}
	cfg := call(20, "tools/call", map[string]any{"name": "firefighter_config", "arguments": map[string]any{"action": "set", "key": "budget.daily_minutes", "value": "99999"}})
	if cfg["result"].(map[string]any)["isError"] != true {
		t.Fatalf("agents must not be able to raise budgets by default: %v", cfg)
	}
	badCwd := call(22, "tools/call", map[string]any{"name": "firefighter_submit", "arguments": map[string]any{"command": cmdline, "cwd": "/no/such/dir", "minutes": 1}})
	if badCwd["result"].(map[string]any)["isError"] != true {
		t.Fatal("a missing cwd must be rejected before spawning")
	}
	bad := call(23, "nope", nil)
	if bad["error"] == nil {
		t.Fatal("unknown method must error")
	}
}

func TestSetupTeachesAgentsAndUninstallCleans(t *testing.T) {
	e := newEnv(t, nil)
	h := t.TempDir()
	for _, d := range []string{".claude", ".codex", ".gemini"} {
		_ = os.MkdirAll(filepath.Join(h, d), 0o755)
	}
	_ = os.WriteFile(filepath.Join(h, ".claude", "CLAUDE.md"), []byte("# mine\n"), 0o644)
	extra := []string{"HOME=" + h, "USERPROFILE=" + h, "CLAUDE_CONFIG_DIR=", "CODEX_HOME="}
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

func TestDockerizeLabelsContainers(t *testing.T) {
	argv, name := dockerize([]string{"docker", "run", "--rm", "img"}, "abc")
	if name != "firefighter-abc" || !strings.Contains(strings.Join(argv, " "), "--label firefighter.job=abc") {
		t.Fatalf("%v %s", argv, name)
	}
	_, name = dockerize([]string{"podman.exe", "run", "--name=mine", "img"}, "abc")
	if name != "mine" {
		t.Fatal(name)
	}
	if argv, name := dockerize([]string{"python", "x.py"}, "abc"); name != "" || len(argv) != 2 {
		t.Fatal("non-docker command changed")
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

func TestRogueHeavyProcessIsAdopted(t *testing.T) {
	e := newEnv(t, nil)
	self, _ := os.Executable()
	e.ff("config", "set", "guard.agent_parents", "["+strings.TrimSuffix(filepath.Base(self), ".exe")+"]")
	e.ff("config", "set", "guard.heavy_seconds", "2")
	e.ff("config", "set", "guard.heavy_cpus", "0.5")
	e.status()
	// Started straight from the test (an "agent"), not through firefighter, and burning CPU.
	c := exec.Command(self)
	c.Env = append(os.Environ(), "FF_HELPER=burn:25")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	eventually(t, "adoption", 40*time.Second, func() bool {
		r, _ := e.status()["running"].([]any)
		for _, x := range r {
			if strings.Contains(fmt.Sprint(x), "unmanaged") && strings.Contains(fmt.Sprint(x), "band:3") {
				return true
			}
		}
		return false
	})
}

func TestAPIRejectsBadPIDs(t *testing.T) {
	e := newEnv(t, nil)
	if out, code := e.ff("adopt", "0"); code == 0 {
		t.Fatalf("adopt 0 must fail:\n%s", out)
	}
	if out, code := e.ff("adopt", "-1"); code == 0 {
		t.Fatalf("adopt -1 must fail:\n%s", out)
	}
	// The daemon itself must survive and keep serving.
	if st := e.status(); st["user"] == nil {
		t.Fatal("daemon unhealthy after bad pids")
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

func TestWindowsCmdQuoting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd.exe quoting is Windows-only")
	}
	e := newEnv(t, nil)
	out, code := e.ff("run", "--minutes", "1", "--", "cmd", "/c", `echo "a & b" & echo second`)
	if code != 0 || !strings.Contains(out, `"a & b"`) || !strings.Contains(out, "second") {
		t.Fatalf("cmd /c line mangled: %d\n%s", code, out)
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

// The image is found past valued options docker has that firefighter doesn't list, and flags and
// short-flag clusters don't eat it; rewrites never touch the container's own arguments.
func TestDockerImageAt(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--ipc", "host", "--gpus", "all", "pytorch:latest", "python", "--name", "x"}, "pytorch:latest"},
		{[]string{"-it", "--rm", "--ulimit", "memlock=-1", "img"}, "img"},
		{[]string{"-d", "-e", "A=1", "--device=/dev/kfd", "rocm/pytorch", "--gpus", "all"}, "rocm/pytorch"},
		{[]string{"--group-add", "video", "-v", "/a:/b", "img:1"}, "img:1"},
		{[]string{"-dp", "8080:80", "--gpus", "all", "web"}, "web"},
		{[]string{"-itv", "/a:/b", "-p8080:80", "-q", "img2"}, "img2"},
	}
	for _, c := range cases {
		if got := dockerImage(c.args); got != c.want {
			t.Fatalf("%v: image %q, want %q", c.args, got, c.want)
		}
	}
	two := 2
	out := dockerGPU([]string{"docker", "run", "--ipc", "host", "--gpus", "all", "img", "--gpus", "all"}, &two, "nvidia")
	if out[5] != "device=2" || out[8] != "all" {
		t.Fatalf("narrow only docker's own --gpus: %v", out)
	}
	argv, name := dockerize([]string{"docker", "run", "--ipc", "host", "--name", "mine", "img", "--name", "inner"}, "abcd1234")
	if name != "mine" {
		t.Fatalf("the user's --name before the image is the container's name: %q %v", name, argv)
	}
}
