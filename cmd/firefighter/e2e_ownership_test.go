package main

// End-to-end process adoption and job ownership checks.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/policy"
)

func TestRogueHeavyProcessIsAdopted(t *testing.T) {
	e := newEnv(t, nil)
	self, _ := os.Executable()
	e.ff("config", "set", "guard.agent_parents", "["+strings.TrimSuffix(filepath.Base(self), ".exe")+"]")
	e.ff("config", "set", "guard.auto_adopt", "true") // off by default in tests: this one adopts its own helper
	e.ff("config", "set", "guard.heavy_seconds", "2")
	e.ff("config", "set", "guard.heavy_cpus", "0.2") // the suite may run under a real guard that squeezes it
	e.status()
	// Started straight from the test (an "agent"), not through firefighter, and burning CPU.
	c := exec.Command(self)
	c.Env = append(os.Environ(), "FF_HELPER=burn:70")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	eventually(t, "adoption", 60*time.Second, func() bool {
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

// A job id belongs to the client that ran it: another client can't reuse it (and take over its
// receipt and waiters). A running process the guard never granted can't re-register as a normal
// job: it's taken in as adopted outside work (spot band).
func TestJobIDsAndReRegistrationAreOwned(t *testing.T) {
	e := newEnv(t, nil)
	id := e.detach("sleep:200", "--minutes", "1")
	if out, code := e.ff("wait", id, "--timeout", "60"); code != 0 {
		t.Fatalf("first job: %d\n%s", code, out)
	}
	if out, code := e.job("sleep:100", "--id", id, "--minutes", "1"); code == 0 || !strings.Contains(out, "belongs to another client") {
		t.Fatalf("reusing another client's id must fail: %d\n%s", code, out)
	}
	t.Setenv("FIREFIGHTER_HOME", e.home)
	c, err := client.Connect()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "FF_HELPER=sleep:20000:"+filepath.Join(e.home, "fake-reg.txt"))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	v, err := c.Create(daemon.Request{Ticket: policy.Ticket{Project: "p", Name: "fake", Minutes: 5, PID: child.Process.Pid}, Running: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Band != policy.BandSpot {
		t.Fatalf("a re-registration the guard never granted must be adopted at the spot band, got band %d", v.Band)
	}
}
