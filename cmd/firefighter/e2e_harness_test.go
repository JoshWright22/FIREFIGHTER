package main

// End-to-end harness: temporary daemons, scripted resources, and job payloads.

// End-to-end tests: build the real binary, start a real daemon in a temp home, and script the
// human, memory pressure and a GPU through FIREFIGHTER_FAKE_FILE. The test binary doubles as the
// job payload (FF_HELPER). CI runs these on Windows, macOS and Linux VMs.

import (
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

var hog []byte // FF_HOG_MB: RAM the payload really holds, so memory rules have something to act on

// helper is the job payload. Modes: exit:N | sleep:MS:FILE | pausable:SECONDS:FILE
func helper(spec string) int {
	if mb, _ := strconv.Atoi(os.Getenv("FF_HOG_MB")); mb > 0 {
		hog = make([]byte, mb<<20)
		// Random bytes, not one per page: macOS compresses mostly-zero pages, and the job then
		// barely shows any RAM (the RAM-pressure test failed on macOS runners only).
		rng := mrand.New(mrand.NewSource(1))
		rng.Read(hog)
		// And keep touching it, as a real job does: macOS swaps or compresses memory a process
		// hasn't used lately (the CI runner showed 0.2 GB resident of 600 MB).
		go func() {
			for {
				for i := 0; i < len(hog); i += 4096 {
					hog[i]++
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}
	f := strings.Split(spec, ":")
	switch f[0] {
	case "exit":
		fmt.Println("helper-out")
		n, _ := strconv.Atoi(f[1])
		return n
	case "leaveholder": // exit at once, leaving a child that keeps our stdout open for 30 s (a build server)
		self, _ := os.Executable()
		c := exec.Command(self)
		c.Env = append(os.Environ(), "FF_HELPER=sleep:30000:"+filepath.Join(os.TempDir(), "ff-holder.txt"))
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		_ = c.Start()
		return 0
	case "launch": // start `firefighter run FF_LAUNCH_ARGS` and exit at once, orphaning it (a killed runner)
		c := exec.Command(os.Getenv("FF_BIN"), strings.Split(os.Getenv("FF_LAUNCH_ARGS"), "\x1f")...)
		c.Env = append(os.Environ(), "FF_HELPER=exit:0")
		if err := c.Start(); err != nil {
			return 98
		}
		time.Sleep(3 * time.Second) // alive while the run starts watching it, as a real launcher is; then gone
		return 0
	case "cudaoom": // fail the way PyTorch does when the GPU runs out of memory
		fmt.Fprintln(os.Stderr, "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB")
		return 1
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
	t     *testing.T
	home  string
	fake  string
	vars  []string
	extra []string // more environment for the jobs this test starts (FF_HOG_MB)
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
	// A test daemon scans the real machine: it must never adopt real agents' work.
	// Admission must not depend on how much RAM and disk the test machine happens to have free: no
	// RAM margin, a small default need, no disk reserve (the tests of those rules turn them on).
	toml := "[guard]\nauto_adopt = false\n[gpu]\ndefault_ram_gb = 0.5\n[reserve]\nram_margin_pct = 0\ndisk_free_gb = 0\n"
	if err := os.WriteFile(filepath.Join(home, "firefighter.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
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
	// A cancelled job's runner can still hold its log open for a moment, and Windows can't delete an
	// open file: retry before the test's TempDir cleanup fails the test over it.
	logs := filepath.Join(e.home, "logs")
	for i := 0; i < 50; i++ {
		if os.RemoveAll(logs) == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *env) setFake(m map[string]any) {
	// Every update keeps the scripted human: a real fullscreen app on the test machine must never leak in.
	if _, ok := m["idle"]; !ok {
		m["idle"] = 9999.0
	}
	if _, ok := m["foreground"]; !ok {
		m["foreground"] = ""
	}
	// No real GPU either unless the test scripts one: a training job running on the test machine's
	// card would read as the human gaming (CI machines have none).
	if _, ok := m["gpus"]; !ok {
		m["gpus"] = []any{}
	}
	// And scripted free RAM (half the machine): a test machine that is itself short of RAM (a build
	// running beside the tests) must not make jobs wait or get suspended.
	if _, ok := m["ram_avail_gb"]; !ok {
		if v, err := mem.VirtualMemory(); err == nil {
			m["ram_avail_gb"] = float64(v.Total) / (1 << 30) / 2
		}
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
	out, err := e.cmd(append([]string{"FF_HELPER=" + helperSpec}, e.extra...), full...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *env) log() string { return readFile(filepath.Join(e.home, "daemon.log")) }

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

func (e *env) mustWaitState(id string) string {
	out, _ := e.ff("wait", id, "--timeout", "1")
	return out
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
