package storage

import (
	"os"
	"path/filepath"
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
