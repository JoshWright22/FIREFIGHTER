package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// Review round 6 (mutation mindset): each safety condition below had no test that failed when it
// was inverted.

func TestRelPathTable(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]string{
		"a": "a", "a/b": "a/b", "./a": "a", "a/./b": "a/b", "a/../b": "b",
		"..": "", "../x": "", "a/../..": "", ".": "", "": "", "/abs": "", "C:x": "", "a:b": "", "./00:/../..": "",
	} {
		got, err := relPath(dir, in)
		if want == "" {
			if err == nil {
				t.Errorf("relPath(%q) = %q, want a refusal", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("relPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := relPath(dir, filepath.Join(dir, "x", "y")); err != nil || got != "x/y" {
		t.Errorf("absolute path inside the space: %q, %v", got, err)
	}
}

func TestFreeRefusesAnUnmarkedRoot(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, err := p.Claim(home, "alpha", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "tmp.bin"), 1<<20)
	if _, err := p.SetLabel(home, "alpha", "tmp.bin", Scratch, "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.Root, marker)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Free(home, "alpha", 100, Useful, false); err == nil {
		t.Fatal("freed files in a root firefighter didn't mark")
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp.bin")); err != nil {
		t.Fatal("a file in an unmarked root was deleted")
	}
	if _, err := p.Release(home, "alpha", true); err == nil {
		t.Fatal("released (deleting files) in an unmarked root")
	}
}

func TestKeepAncestorProtectsScratchChild(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, _ := p.Claim(home, "alpha", 5, "")
	write(t, filepath.Join(dir, "best", "tmp", "x.bin"), 1<<20)
	if _, err := p.SetLabel(home, "alpha", "best", Keep, "the result"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetLabel(home, "alpha", "best/tmp", Scratch, "looks like scratch"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Free(home, "alpha", 100, Useful, false); err != nil {
		t.Logf("free: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "best", "tmp", "x.bin")); err != nil {
		t.Fatal("a scratch label under a keep folder let Free delete it")
	}
}
