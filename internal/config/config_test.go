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
