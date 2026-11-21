package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Review round 6 (Opus): the real state folder named another way is still the real one, so the
// test-only overrides stay off there.
func TestDefaultHomeByIdentity(t *testing.T) {
	base := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", base)
	case "darwin":
		t.Setenv("HOME", base)
		base = filepath.Join(base, "Library", "Application Support")
	default:
		t.Setenv("XDG_STATE_HOME", base)
	}
	real := filepath.Join(base, "firefighter")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(real))
	for _, h := range []string{real, "firefighter", "./firefighter/", filepath.Join(real, "..", "firefighter")} {
		t.Setenv("FIREFIGHTER_HOME", h)
		if !IsDefaultHome() {
			t.Errorf("%q names the real folder but isn't treated as it", h)
		}
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err == nil {
		t.Setenv("FIREFIGHTER_HOME", link)
		if !IsDefaultHome() {
			t.Error("a link to the real folder isn't treated as it")
		}
	}
	t.Setenv("FIREFIGHTER_HOME", t.TempDir())
	if IsDefaultHome() {
		t.Error("a test folder is treated as the real one")
	}
}
