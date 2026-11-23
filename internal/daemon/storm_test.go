package daemon

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// 2026-11-23: a git hook that called itself grew to 4,900 sh.exe and free RAM fell to 0.1 GB while
// the human gamed; nothing acted. A storm is reported while RAM has room, and stopped once RAM is
// under the human's reserve.
func TestProcessStormIsStoppedWhenRAMRunsShort(t *testing.T) {
	d := testDaemon(t)
	const n = 60
	var cmds []*exec.Cmd
	for i := 0; i < n; i++ {
		c := exec.Command("sleep", "60")
		if runtime.GOOS == "windows" {
			c = exec.Command("ping", "-n", "60", "127.0.0.1")
		}
		if err := c.Start(); err != nil {
			t.Skipf("can't start the test's processes: %v", err)
		}
		cmds = append(cmds, c)
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	time.Sleep(500 * time.Millisecond)
	d.storms = sysinfo.Storms(sysinfo.StormParams{MinCount: 50, MinGB: 0, Within: 10 * time.Minute}, nil)
	if len(d.storms) != 1 || len(d.storms[0].PIDs) < n {
		t.Fatalf("the %d identical processes weren't found as one storm: %+v", n, d.storms)
	}
	keep := d.storms
	d.snap.RAMAvailGB = 20 // room: only reported
	d.handleStorms(time.Now())
	if n := strings.Join(d.notes(time.Now()), "\n"); !strings.Contains(n, "process storm:") {
		t.Fatalf("notes: %s", n)
	}
	if cmds[0].ProcessState != nil || !sysinfo.Alive(cmds[0].Process.Pid) {
		t.Fatal("a storm was stopped with RAM to spare")
	}
	d.storms = keep
	d.snap.RAMAvailGB = 0.1 // under the reserve: stopped
	d.handleStorms(time.Now())
	deadline := time.Now().Add(10 * time.Second)
	for _, c := range cmds {
		for sysinfo.Alive(c.Process.Pid) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if sysinfo.Alive(c.Process.Pid) {
			t.Fatalf("storm process %d survived", c.Process.Pid)
		}
	}
	if !strings.Contains(daemonLog(t), "process storm stopped (RAM is under the human's reserve): killed") {
		t.Fatalf("log: %s", daemonLog(t))
	}
}
