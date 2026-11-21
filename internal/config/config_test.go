package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetUnsetRoundTrip(t *testing.T) {
	t.Setenv("FIREFIGHTER_HOME", t.TempDir())
	if err := Set("order.policy", "drf"); err != nil {
		t.Fatal(err)
	}
	if err := Set("budget.projects.myrepo", "90"); err != nil {
		t.Fatal(err)
	}
	if err := Set("reserve.game.cpus", "12"); err != nil {
		t.Fatal(err)
	}
	if err := Set("reserve.exclusive_apps", "[obs64.exe, blender]"); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Order.Policy != "drf" || c.Allowance("myrepo") != 90 || c.Reserve.Game.CPUs != 12 || len(c.Reserve.ExclusiveApps) != 2 {
		t.Fatalf("not applied: %+v", c)
	}
	if c.Reserve.Game.RAMPct != 15 {
		t.Fatal("setting one game key must keep the others' defaults")
	}
	if err := Unset("order.policy"); err != nil {
		t.Fatal(err)
	}
	c, _ = Load()
	if c.Order.Policy != "las" {
		t.Fatalf("unset should restore default, got %s", c.Order.Policy)
	}
}

func TestRejectsBadValues(t *testing.T) {
	t.Setenv("FIREFIGHTER_HOME", t.TempDir())
	if err := Set("order.policy", "chaos"); err == nil {
		t.Fatal("unknown policy accepted")
	}
	if err := Set("preset", "nope"); err == nil {
		t.Fatal("unknown preset accepted")
	}
	if err := Set("gpu.max_jobs", "0"); err == nil {
		t.Fatal("max_jobs 0 accepted")
	}
	for _, v := range []string{"0", "1", "-5"} { // overdue jobs start while the human is at the keyboard
		if err := Set("order.max_wait_minutes", v); err == nil {
			t.Fatalf("max_wait_minutes %s accepted", v)
		}
	}
	if err := Set("order.max_wait_minutes", "30"); err != nil {
		t.Fatalf("a normal max_wait refused: %v", err)
	}
	if _, err := os.Stat(Path()); err == nil {
		b, _ := os.ReadFile(Path())
		if strings.Contains(string(b), "chaos") {
			t.Fatal("bad value was written")
		}
	}
}

func TestPresetsThenOverrides(t *testing.T) {
	t.Setenv("FIREFIGHTER_HOME", t.TempDir())
	if err := Set("preset", "gamer"); err != nil {
		t.Fatal(err)
	}
	c, _ := Load()
	if c.GPU.MaxJobs != 1 || c.Reserve.Game.CPUs != 10 {
		t.Fatalf("gamer preset not applied: %+v", c.GPU)
	}
	_ = Set("gpu.max_jobs", "2")
	c, _ = Load()
	if c.GPU.MaxJobs != 2 {
		t.Fatal("override must beat the preset")
	}
	for name := range Presets {
		raw := map[string]any{"preset": name}
		if _, err := build(raw); err != nil {
			t.Errorf("preset %s invalid: %v", name, err)
		}
	}
}

func TestBrokenFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FIREFIGHTER_HOME", dir)
	_ = os.WriteFile(filepath.Join(dir, "firefighter.toml"), []byte("order = = broken"), 0o644)
	c, err := Load()
	if err == nil || c.Order.Policy != "las" {
		t.Fatalf("broken file: err=%v policy=%s", err, c.Order.Policy)
	}
}

// Review round 3 (Fable): NaN makes every limit comparison false (unlimited storage claims).
func TestNaNIsRefused(t *testing.T) {
	t.Setenv("FIREFIGHTER_HOME", t.TempDir())
	for _, kv := range [][2]string{{"storage.gb", "nan"}, {"storage.projects.alpha", "NaN"}, {"reserve.min_free_gb", "inf"}} {
		if err := Set(kv[0], kv[1]); err == nil {
			t.Errorf("config set %s %s accepted", kv[0], kv[1])
		}
	}
	if err := Set("storage.gb", "50"); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.Storage.GB != 50 {
		t.Fatalf("load after set: %v %v", c.Storage.GB, err)
	}
}
