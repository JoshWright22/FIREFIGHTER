package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pool(t *testing.T, gb, free float64) (Pool, string) {
	t.Helper()
	dir := t.TempDir()
	return Pool{Root: filepath.Join(dir, "spaces"), GB: gb, Caps: map[string]float64{"capped": 2},
		FloorGB: func(string) (float64, float64, bool) { return free, 10, true }}, filepath.Join(dir, "home")
}

func write(t *testing.T, path string, bytes int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, bytes), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaimWithinAllotment(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, err := p.Claim(home, "alpha", 6, "datasets")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("space folder not made: %v", err)
	}
	if _, err := p.Claim(home, "beta", 5, ""); err == nil || !strings.Contains(err.Error(), "4.0 GB of the 10.0 GB allotment is unclaimed") || !strings.Contains(err.Error(), "alpha 6.0") {
		t.Fatalf("over-allotment claim: %v", err)
	}
	if _, err := p.Claim(home, "beta", 4, ""); err != nil {
		t.Fatal(err)
	}
	// alpha may shrink, and then beta may grow into what it gave back
	if _, err := p.Claim(home, "alpha", 3, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Claim(home, "beta", 7, ""); err != nil {
		t.Fatal(err)
	}
	g, _ := Grants(home)
	if g["alpha"].GB != 3 || g["beta"].GB != 7 || g["alpha"].Why != "datasets" {
		t.Fatalf("grants: %+v", g)
	}
}

func TestClaimRules(t *testing.T) {
	p, home := pool(t, 0, 500)
	if _, err := p.Claim(home, "alpha", 1, ""); err != ErrNoAllotment {
		t.Fatalf("no allotment: %v", err)
	}
	p, home = pool(t, 100, 500)
	if _, err := p.Claim(home, "capped", 3, ""); err == nil || !strings.Contains(err.Error(), "at most 2.0 GB") {
		t.Fatalf("human's per-project cap: %v", err)
	}
	for _, bad := range []string{"..", "a/b", `a\b`, "", "-x"} {
		if _, err := p.Claim(home, bad, 1, ""); err == nil {
			t.Fatalf("project %q accepted", bad)
		}
	}
	// the drive must hold the claimed room beyond the human's floor: 30 free - 10 kept = 20
	p, home = pool(t, 100, 30)
	if _, err := p.Claim(home, "alpha", 25, ""); err == nil || !strings.Contains(err.Error(), "keeps 10 GB for the human") {
		t.Fatalf("drive room: %v", err)
	}
	if _, err := p.Claim(home, "alpha", 15, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Claim(home, "beta", 6, ""); err == nil {
		t.Fatal("second claim fits the allotment but not the drive: accepted")
	}
}

func TestShrinkBelowUseAndRelease(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, err := p.Claim(home, "alpha", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "sub", "data.bin"), 3<<20)
	p.GB = 10
	if _, err := p.Claim(home, "alpha", 0.001, ""); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("shrink below use: %v", err)
	}
	if _, err := p.Release(home, "alpha", false); err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Fatalf("release with files, no --delete: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("files removed without --delete")
	}
	freed, err := p.Release(home, "alpha", true)
	if err != nil || freed <= 0 {
		t.Fatalf("release --delete: %v %v", freed, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("space folder left behind")
	}
	if g, _ := Grants(home); len(g) != 0 {
		t.Fatalf("grant left: %+v", g)
	}
	if _, err := os.Stat(p.Root); err != nil {
		t.Fatal("the root itself was removed")
	}
}

func TestLookFlagsOverAndUnclaimed(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, _ := p.Claim(home, "alpha", 1, "")
	write(t, filepath.Join(dir, "big.bin"), 2<<20)
	write(t, filepath.Join(p.Root, "stray", "x.bin"), 1<<20)
	r, err := p.Look(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Spaces) != 2 || r.Spaces[0].Project != "alpha" || r.Spaces[1].Project != "stray" {
		t.Fatalf("spaces: %+v", r.Spaces)
	}
	if r.Spaces[0].Over || r.Spaces[0].Files != 1 {
		t.Fatalf("alpha holds 2 MB of 1 GB: %+v", r.Spaces[0])
	}
	if r.Spaces[1].GB != 0 || r.Spaces[1].Files != 1 {
		t.Fatalf("stray: %+v", r.Spaces[1])
	}
	if r.GrantedGB != 1 || r.DriveFree != 500 || r.FloorGB != 10 {
		t.Fatalf("report: %+v", r)
	}
}

func TestRemoveSpaceStaysInRoot(t *testing.T) {
	root := t.TempDir()
	if err := removeSpace(root, filepath.Join(root, "a", "b")); err == nil {
		t.Fatal("deleted a folder that isn't a space")
	}
	if err := removeSpace(root, filepath.Dir(root)); err == nil {
		t.Fatal("deleted the root's parent")
	}
}

// storage.root pointed at a folder of the human's: no claim, nothing listed, nothing deleted.
func TestRootMustBeNewOrEmpty(t *testing.T) {
	p, home := pool(t, 10, 500)
	write(t, filepath.Join(p.Root, "Documents", "taxes.pdf"), 10)
	if _, err := p.Claim(home, "alpha", 1, ""); err == nil || !strings.Contains(err.Error(), "already holds other files") {
		t.Fatalf("claim in a folder of the human's: %v", err)
	}
	if _, err := p.Release(home, "Documents", true); err == nil {
		t.Fatal("released (deleted) a folder of the human's")
	}
	if _, err := os.Stat(filepath.Join(p.Root, "Documents", "taxes.pdf")); err != nil {
		t.Fatal("the human's file is gone")
	}
	if r, _ := p.Look(home); len(r.Spaces) != 0 {
		t.Fatalf("listed the human's folders: %+v", r.Spaces)
	}
}

func TestProjectNamesIgnoreCase(t *testing.T) {
	p, home := pool(t, 10, 500)
	if _, err := p.Claim(home, "Alpha", 4, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Claim(home, "alpha", 6, ""); err != nil {
		t.Fatalf("the same space in another case should resize it: %v", err)
	}
	if g, _ := Grants(home); len(g) != 1 || g["Alpha"].GB != 6 {
		t.Fatalf("grants: %+v", g)
	}
	if _, err := p.Release(home, "ALPHA", false); err != nil {
		t.Fatal(err)
	}
}
