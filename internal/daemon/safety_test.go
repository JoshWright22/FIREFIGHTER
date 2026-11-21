package daemon

import (
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review round 6 (Fable): sizes from docker inspect or a --memory flag are agent text.
func TestMemSizeRejectsNonsense(t *testing.T) {
	for in, want := range map[string]float64{"4g": 4, "512m": 0.5, "4gb": 4, "1073741824": 1, "": 0, "-4g": 0,
		"nan": 0, "NaNg": 0, "infg": 0, "+Inf": 0, "1e300g": 0, "2000000g": 0, "abc": 0} {
		got := memSize(in)
		if math.IsNaN(got) || math.Abs(got-want) > 1e-9 {
			t.Errorf("memSize(%q) = %v, want %v", in, got, want)
		}
	}
}

// A damaged state.json is set aside and the guard starts clean, instead of running on part of it.
func TestDamagedStateIsSetAside(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FIREFIGHTER_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"accounts": {"a": {"spent": 1}}, "history": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := load()
	if len(s.Accounts) != 0 || s.History == nil || s.Receipts == nil {
		t.Fatalf("partial state kept: %+v", s.Accounts)
	}
	bad, _ := filepath.Glob(filepath.Join(home, "state.json.bad-*"))
	if len(bad) != 1 {
		t.Fatalf("damaged file not kept aside: %v", bad)
	}
}

// A reply that can't be encoded is a 500, not a 200 with half a body.
func TestReplyEncodeErrorIs500(t *testing.T) {
	w := httptest.NewRecorder()
	reply(w, map[string]any{"x": math.NaN()})
	if w.Code != 500 {
		t.Fatalf("code %d, body %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	reply(w, map[string]int{"x": 1})
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"x":1}` {
		t.Fatalf("code %d, body %q", w.Code, w.Body.String())
	}
}

// Review round 6 (Opus): --evict never doesn't let an agent keep the human's VRAM reserve forever.
// Short pressure leaves a never job alone; pressure lasting three periods asks it to checkpoint.
func TestEvictNeverYieldsToLongVRAMPressure(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	j := d.addRunning("never job", 2, now.Add(-30*time.Minute))
	j.Evict, j.vramGB = "never", 12
	d.snap.GPUs[0].UsedGB = 15.8 // far under the human's VRAM reserve
	p := time.Duration(d.cfg.Ladder.PressureSeconds * float64(time.Second))
	d.vramLow = now.Add(-2 * p)
	d.ladder(now)
	if !j.checkpoint.IsZero() {
		t.Fatal("a never job was asked to leave on short pressure")
	}
	d.vramLow = now.Add(-4 * p)
	d.ladder(now)
	if j.checkpoint.IsZero() {
		t.Fatalf("a never job kept VRAM through long pressure (reserve %.1f GB)\n%s", d.res.VRAMGB, daemonLog(t))
	}
}

// A damaged state.json still thaws what the dead guard froze.
func TestSalvageKeepsFrozen(t *testing.T) {
	s := salvage([]byte(`{"frozen": {"4242": 77}, "accounts": {"a": {"spent": 1}}, "history": [`))
	if s.Frozen[4242] != 77 || len(s.Accounts) != 0 {
		t.Fatalf("salvage: %+v", s)
	}
	if s := salvage([]byte(`garbage`)); s.Frozen != nil {
		t.Fatal("salvaged from garbage")
	}
}

// Every endpoint sits behind the host and token checks (review round 6: no test caught either
// being removed).
func TestHostAndTokenChecks(t *testing.T) {
	d := testDaemon(t)
	h := d.routes()
	try := func(host, token string) int {
		r := httptest.NewRequest("GET", "/v1/no-such-route", nil)
		r.Host = host
		if token != "" {
			r.Header.Set("X-Firefighter-Token", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := try("127.0.0.1:5000", ""); c != 403 {
		t.Fatalf("no token set on the guard: %d, want 403", c)
	}
	d.Token = "secret"
	for _, c := range []struct {
		host, token string
		want        int
	}{
		{"127.0.0.1:5000", "secret", 404}, {"localhost", "secret", 404}, {"[::1]:9", "secret", 404},
		{"127.0.0.1:5000", "", 403}, {"127.0.0.1:5000", "wrong", 403},
		{"evil.example:5000", "secret", 403}, {"127.0.0.1.evil.example", "secret", 403},
	} {
		if got := try(c.host, c.token); got != c.want {
			t.Errorf("host %q token %q: %d, want %d", c.host, c.token, got, c.want)
		}
	}
}
