package storage

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A space with: results (keep), a downloaded dataset (useful), a tmp folder (scratch) holding one
// file someone still needs (keep, deeper label wins), an old and a new unranked file.
func rankedSpace(t *testing.T) (Pool, string, string) {
	t.Helper()
	p, home := pool(t, 10, 500)
	dir, err := p.Claim(home, "alpha", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "results", "run1.json"), 1<<20)
	write(t, filepath.Join(dir, "data", "train.bin"), 3<<20)
	write(t, filepath.Join(dir, "tmp", "a.bin"), 2<<20)
	write(t, filepath.Join(dir, "tmp", "keepme", "ckpt.pt"), 2<<20)
	write(t, filepath.Join(dir, "old.log"), 1<<20)
	write(t, filepath.Join(dir, "new.log"), 1<<20)
	old := time.Now().Add(-72 * time.Hour)
	os.Chtimes(filepath.Join(dir, "old.log"), old, old)
	for _, l := range []struct{ path, rank, why string }{
		{"results", Keep, "final numbers for the report"},
		{filepath.Join(dir, "data"), Useful, "training set, 2 h to download"},
		{"tmp", Scratch, "intermediate shards"},
		{"tmp/keepme", Keep, "best checkpoint"},
	} {
		if _, err := p.SetLabel(home, "alpha", l.path, l.rank, l.why); err != nil {
			t.Fatalf("label %s: %v", l.path, err)
		}
	}
	return p, home, dir
}

func TestItemsLeastImportantFirst(t *testing.T) {
	p, home, _ := rankedSpace(t)
	its, err := p.Items(home, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range its {
		got = append(got, it.Rank+":"+it.Path)
	}
	want := "scratch:tmp unranked:old.log unranked:new.log useful:data keep:results keep:tmp/keepme"
	if strings.Join(got, " ") != want {
		t.Fatalf("order\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if its[0].Files != 1 || its[0].For != "intermediate shards" {
		t.Fatalf("tmp should own only a.bin (keepme has its own label): %+v", its[0])
	}
	r, _ := p.Look(home)
	if _, ok := r.Spaces[0].RankGB[Keep]; !ok || len(r.Spaces[0].RankGB) != 4 {
		t.Fatalf("rank totals: %+v", r.Spaces[0].RankGB)
	}
}

func TestLabelRules(t *testing.T) {
	p, home, dir := rankedSpace(t)
	if _, err := p.SetLabel(home, "alpha", "results", "keep", ""); err == nil {
		t.Fatal("a label must say what it is for")
	}
	if _, err := p.SetLabel(home, "alpha", "results", "important", "x"); err == nil {
		t.Fatal("unknown rank accepted")
	}
	if _, err := p.SetLabel(home, "alpha", "../beta", "keep", "x"); err == nil {
		t.Fatal("a path outside the space accepted")
	}
	if _, err := p.SetLabel(home, "alpha", filepath.Join(filepath.Dir(dir), "x"), "keep", "x"); err == nil {
		t.Fatal("an absolute path outside the space accepted")
	}
	if _, err := p.SetLabel(home, "alpha", "missing.bin", "keep", "x"); err == nil {
		t.Fatal("a missing path accepted")
	}
	if _, err := p.SetLabel(home, "alpha", "results", "none", ""); err != nil {
		t.Fatal(err)
	}
	if g, _ := Grants(home); g["alpha"].Labels["results"].Rank != "" {
		t.Fatal("rank none should remove the label")
	}
}

func TestFreeDeletesLeastImportantNeverKeep(t *testing.T) {
	p, home, dir := rankedSpace(t)
	// dry run: nothing goes
	chosen, _, err := p.Free(home, "alpha", 0.001, "", true)
	if err != nil || len(chosen) != 1 || chosen[0].Path != "tmp" {
		t.Fatalf("dry run: %+v %v", chosen, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "a.bin")); err != nil {
		t.Fatal("dry run deleted")
	}
	// scratch only (the default): tmp/a.bin goes, tmp/keepme stays
	chosen, freed, err := p.Free(home, "alpha", 100, "", false)
	if err != nil || len(chosen) != 1 || freed < 0.001 {
		t.Fatalf("free scratch: %+v %.3f %v", chosen, freed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "a.bin")); !os.IsNotExist(err) {
		t.Fatal("scratch file left")
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "keepme", "ckpt.pt")); err != nil {
		t.Fatal("a keep file inside a scratch folder was deleted")
	}
	// up to useful, a lot: everything but keep goes, oldest unranked first
	chosen, _, err = p.Free(home, "alpha", 100, Useful, false)
	if err != nil || len(chosen) != 3 || chosen[0].Path != "old.log" || chosen[2].Path != "data" {
		t.Fatalf("free up to useful: %+v %v", chosen, err)
	}
	for _, f := range []string{"results/run1.json", "tmp/keepme/ckpt.pt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Fatalf("keep file %s deleted", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatal("emptied folder left behind")
	}
	if g, _ := Grants(home); g["alpha"].Labels["data"].Rank != "" || g["alpha"].Labels["results"].Rank != Keep {
		t.Fatalf("labels after free: %+v", g["alpha"].Labels)
	}
	if _, _, err := p.Free(home, "alpha", 1, Keep, false); err == nil {
		t.Fatal("--upto keep accepted")
	}
}

// Review round 2 (GPT): a folder swapped for a junction (Windows) or link after the walk must not
// lead a delete outside the space.
func TestRemoveFilesRefusesASwappedJunction(t *testing.T) {
	base := t.TempDir()
	space, outside := filepath.Join(base, "space"), filepath.Join(base, "outside")
	write(t, filepath.Join(space, "tmp", "a.bin"), 10)
	write(t, filepath.Join(outside, "a.bin"), 10)           // the human's file with the same name
	files := []string{filepath.Join(space, "tmp", "a.bin")} // what the walk saw
	if err := os.RemoveAll(filepath.Join(space, "tmp")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(space, "tmp")
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", link, outside).Run()
	} else {
		err = os.Symlink(outside, link)
	}
	if err != nil {
		t.Skipf("can't make a link here: %v", err)
	}
	rt, err := os.OpenRoot(space)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	gone, left := removeFiles(rt, space, files)
	if _, err := os.Stat(filepath.Join(outside, "a.bin")); err != nil {
		t.Fatal("a file outside the space was deleted through a swapped-in link")
	}
	if gone != 0 || left != 1 {
		t.Fatalf("gone %d left %d", gone, left)
	}
}

// Review round 2 (GPT): an empty folder ranked keep survives free and blocks release --delete; two
// labels on one file in different case keep the more protective one; a NaN target is refused.
func TestKeepEmptyFolderDuplicateLabelsAndNaN(t *testing.T) {
	p, home, dir := rankedSpace(t)
	if err := os.MkdirAll(filepath.Join(dir, "tmp", "empty-keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetLabel(home, "alpha", "tmp/empty-keep", Keep, "mount point a job expects"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Free(home, "alpha", 100, Scratch, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "empty-keep")); err != nil {
		t.Fatal("an empty folder ranked keep was pruned")
	}
	if _, err := p.SetLabel(home, "alpha", "results", "none", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetLabel(home, "alpha", "tmp/keepme", "none", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Release(home, "alpha", true); err == nil || !strings.Contains(err.Error(), "empty-keep") {
		t.Fatalf("release --delete with an empty keep folder: %v", err)
	}
	if _, _, err := p.Free(home, "alpha", math.NaN(), Useful, false); err == nil {
		t.Fatal("NaN accepted")
	}
	labels := map[string]Label{"data": {Rank: Keep, For: "x"}, "DATA": {Rank: Scratch, For: "y"}}
	for i := 0; i < 20 && foldCase; i++ { // map order varies: keep must win every time
		for _, it := range items(dir, labels) {
			if strings.EqualFold(it.Path, "data") && it.Files > 0 && it.Rank != Keep {
				t.Fatalf("duplicate labels: %+v", it)
			}
		}
	}
}

// Review round 3 (Fable): an empty folder under a folder ranked keep is kept too, and a space whose
// only content is an empty folder ranked keep is not released.
func TestKeepCoversWhatIsUnderIt(t *testing.T) {
	p, home := pool(t, 10, 500)
	dir, _ := p.Claim(home, "alpha", 5, "")
	if err := os.MkdirAll(filepath.Join(dir, "a", "b", "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "a", "c.tmp"), 10)
	if _, err := p.SetLabel(home, "alpha", "a/b", Keep, "outputs go here"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Free(home, "alpha", 1, Unranked, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "b", "out")); err != nil {
		t.Fatal("an empty folder under a keep folder was pruned")
	}
	if _, err := p.Release(home, "alpha", false); err == nil {
		t.Fatal("released a space whose keep folder still exists")
	}
}
