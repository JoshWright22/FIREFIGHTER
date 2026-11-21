package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Ranks, most important first. An item no agent ranked is "unranked": it sits between useful and
// scratch when space must be freed.
const (
	Keep     = "keep"     // needed: results, the only copy of something, what a running job reads
	Useful   = "useful"   // costly to remake (a download, a built cache), but it can be remade
	Unranked = "unranked" // nobody said
	Scratch  = "scratch"  // temporary: delete first
)

// Ranks is the deletion order: what goes first when space is freed. Keep is never deleted.
var Ranks = []string{Scratch, Unranked, Useful, Keep}

func rankOrder(r string) int {
	for i, x := range Ranks {
		if x == r {
			return i
		}
	}
	return 1
}

// Label is what one file or folder in a space is for.
type Label struct {
	Rank string    `json:"rank"`
	For  string    `json:"for"`
	At   time.Time `json:"at"`
}

// Item is one ranked path in a space (or, for files no label covers, one top-level entry).
type Item struct {
	Path     string    `json:"path"` // inside the space, "/" separators
	Rank     string    `json:"rank"`
	For      string    `json:"for,omitempty"`
	GB       float64   `json:"gb"`
	Files    int       `json:"files"`
	Modified time.Time `json:"modified"`       // newest file in it
	Left     int       `json:"left,omitempty"` // free: files it could not delete (in use)
	files    []string
}

// relPath turns an agent's path (inside the space, or absolute) into a clean path inside it.
func relPath(dir, path string) (string, error) {
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(dir, path)
		if err != nil {
			return "", err
		}
		path = r
	}
	r := filepath.ToSlash(filepath.Clean(path))
	if r == "." || r == ".." || strings.HasPrefix(r, "../") || filepath.IsAbs(r) || strings.Contains(r, ":") {
		return "", fmt.Errorf("%s is not inside the space %s", path, dir)
	}
	return r, nil
}

// SetLabel ranks a file or folder in a project's space and says what it is for. Rank "none" removes
// the label.
func (p Pool) SetLabel(home, project, path, rank, purpose string) (string, error) {
	rank = strings.ToLower(strings.TrimSpace(rank))
	if rank != Keep && rank != Useful && rank != Scratch && rank != "none" {
		return "", fmt.Errorf("rank %q: use keep (needed), useful (costly to remake), scratch (temporary) or none", rank)
	}
	if rank != "none" && strings.TrimSpace(purpose) == "" {
		return "", errors.New(`say what it is for: --for "<what uses it, and until when>"`)
	}
	if _, err := p.Dir(project); err != nil {
		return "", err
	}
	unlock, err := lock(home)
	if err != nil {
		return "", err
	}
	defer unlock()
	f, err := load(home)
	if err != nil {
		return "", err
	}
	project = canonical(f, project)
	g, ok := f.Grants[project]
	if !ok {
		return "", fmt.Errorf("%s has no space: firefighter storage claim --gb N first", project)
	}
	dir := filepath.Join(p.Root, project)
	rel, err := relPath(dir, path)
	if err != nil {
		return "", err
	}
	if rel, err = resolve(dir, rel); err != nil {
		return "", err
	}
	if g.Labels == nil {
		g.Labels = map[string]Label{}
	}
	for other := range g.Labels {
		if other != rel && foldKey(other) == foldKey(rel) {
			delete(g.Labels, other) // one label per file
		}
	}
	if rank == "none" {
		delete(g.Labels, rel)
	} else {
		g.Labels[rel] = Label{Rank: rank, For: text(purpose, 200), At: time.Now()}
	}
	f.Grants[project] = g
	return rel, save(home, f)
}

// Items lists a space's contents by rank: each labeled path (its files that no deeper label covers)
// and each top-level entry no label covers. Least important first: scratch, unranked, useful, keep;
// within a rank, the longest untouched first.
func (p Pool) Items(home, project string) ([]Item, error) {
	if _, err := p.Dir(project); err != nil {
		return nil, err
	}
	f, err := load(home)
	if err != nil {
		return nil, err
	}
	project = canonical(f, project)
	its := items(filepath.Join(p.Root, project), f.Grants[project].Labels)
	for i := range its { // shown to agents and the human: file names are anyone's to choose
		its[i].Path, its[i].For = text(its[i].Path, 1000), text(its[i].For, 200)
	}
	return its, nil
}

// foldCase: Windows and macOS file names ignore case, so labels match whatever case the files have.
var foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

func foldKey(p string) string {
	if foldCase {
		return strings.ToLower(p)
	}
	return p
}

// resolve turns a path inside the space into the exact names on disk, one part at a time, so a
// label typed in another case (or "data." for "data" on Windows) can't miss the files it names.
func resolve(dir, rel string) (string, error) {
	cur, out := dir, []string{}
	for _, part := range strings.Split(rel, "/") {
		es, err := os.ReadDir(cur)
		if err != nil {
			return "", fmt.Errorf("%s: no such file or folder in %s", rel, dir)
		}
		name := ""
		for _, e := range es {
			if e.Name() == part {
				name = part
				break
			}
			if foldCase && strings.EqualFold(e.Name(), part) {
				name = e.Name()
			}
		}
		if name == "" {
			return "", fmt.Errorf("%s: no such file or folder in %s (names must match the files exactly)", rel, dir)
		}
		out = append(out, name)
		cur = filepath.Join(cur, name)
	}
	return strings.Join(out, "/"), nil
}

func items(dir string, labels map[string]Label) []Item {
	byPath := map[string]*Item{}
	byKey := map[string]string{} // foldKey(label path) -> label path
	for path, l := range labels {
		byPath[path] = &Item{Path: path, Rank: l.Rank, For: l.For}
		// Two labels that name one file in different case (set before labels were resolved): the
		// more protective rank wins, so a keep is never lost to a scratch.
		if prev, ok := byKey[foldKey(path)]; !ok || rankOrder(l.Rank) > rankOrder(labels[prev].Rank) {
			byKey[foldKey(path)] = path
		}
	}
	var bytes = map[string]int64{}
	_ = filepath.WalkDir(dir, func(full string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		owner := ""
		for p := rel; p != "." && p != ""; p = parent(p) { // the deepest label covering it
			if l, ok := byKey[foldKey(p)]; ok {
				owner = l
				break
			}
		}
		if owner == "" {
			owner = strings.SplitN(rel, "/", 2)[0]
			if _, ok := byPath[owner]; !ok {
				byPath[owner] = &Item{Path: owner, Rank: Unranked}
			}
		}
		it := byPath[owner]
		if fi, err := e.Info(); err == nil {
			bytes[owner] += fi.Size()
			if fi.ModTime().After(it.Modified) {
				it.Modified = fi.ModTime()
			}
		}
		it.Files++
		it.files = append(it.files, full)
		return nil
	})
	out := make([]Item, 0, len(byPath))
	for path, it := range byPath {
		if it.Files == 0 && it.Rank == Unranked {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			continue // a label on something already deleted
		}
		it.GB = math.Round(float64(bytes[path])/(1<<30)*1000) / 1000
		out = append(out, *it)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if ra, rb := rankOrder(out[a].Rank), rankOrder(out[b].Rank); ra != rb {
			return ra < rb
		}
		if !out[a].Modified.Equal(out[b].Modified) {
			return out[a].Modified.Before(out[b].Modified)
		}
		return out[a].Path < out[b].Path
	})
	return out
}

func parent(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// Free deletes a space's least important items until gb is freed: scratch first, then (when upto
// allows) unranked, then useful; oldest first within a rank. Keep is never deleted. With dryRun it
// only says what it would delete.
func (p Pool) Free(home, project string, gb float64, upto string, dryRun bool) (chosen []Item, freed float64, err error) {
	if upto == "" {
		upto = Scratch
	}
	if upto != Scratch && upto != Unranked && upto != Useful {
		return nil, 0, fmt.Errorf("--upto %q: scratch, unranked or useful (keep is never deleted)", upto)
	}
	if !(gb > 0) || math.IsInf(gb, 0) { // also NaN: it would pass every stop check and delete everything eligible
		return nil, 0, errors.New("--gb must be a number more than 0")
	}
	if _, err := p.Dir(project); err != nil {
		return nil, 0, err
	}
	unlock, err := lock(home)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	f, err := load(home)
	if err != nil {
		return nil, 0, err
	}
	project = canonical(f, project)
	g, ok := f.Grants[project]
	if !ok {
		return nil, 0, fmt.Errorf("%s has no space", project)
	}
	if !p.marked() {
		return nil, 0, fmt.Errorf("%s isn't a storage root firefighter made: nothing deleted", p.Root)
	}
	dir := filepath.Join(p.Root, project)
	// Every delete goes through an os.Root on the space: a path that resolves outside it (a folder
	// swapped for a junction or link after the walk) is refused by the OS-level lookup itself.
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer rt.Close()
	protected := map[string]bool{} // labeled folders that are not being freed: never pruned
	for path, l := range g.Labels {
		if rankOrder(l.Rank) > rankOrder(upto) {
			protected[foldKey(path)] = true
		}
	}
	for _, it := range items(dir, g.Labels) {
		if freed >= gb || rankOrder(it.Rank) > rankOrder(upto) {
			break
		}
		if it.Files == 0 {
			continue // emptied already (what's left under it has deeper labels of its own)
		}
		if dryRun {
			chosen = append(chosen, it)
			freed += it.GB
			continue
		}
		gone, left := removeFiles(rt, dir, it.files)
		it.Left += left
		it.GB = math.Round(float64(gone)/(1<<30)*1000) / 1000
		chosen = append(chosen, it)
		freed += float64(gone) / (1 << 30)
		pruneEmpty(rt, dir, it.Path, protected)
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(it.Path))); err != nil {
			delete(g.Labels, it.Path)
		}
	}
	if !dryRun {
		f.Grants[project] = g
		err = save(home, f)
	}
	return chosen, freed, err
}

// removeFiles deletes plain files (full paths under dir) through rt, the space's os.Root: a path
// that now resolves outside the space is refused by the lookup itself. It returns the bytes really
// removed and how many files it could not remove.
func removeFiles(rt *os.Root, dir string, files []string) (gone int64, left int) {
	for _, file := range files {
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			left++
			continue
		}
		fi, err := rt.Lstat(rel)
		if err != nil {
			left++
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if rt.Remove(rel) == nil {
			gone += fi.Size()
		} else {
			left++ // in use (a running job holds it), or it now resolves outside the space
		}
	}
	return gone, left
}

// pruneEmpty removes the now-empty folders under one item (inside the space, through rt), deepest
// first, never the space itself and never a folder labeled with a rank that is not being freed.
func pruneEmpty(rt *os.Root, dir, item string, protected map[string]bool) {
	var dirs []string
	_ = filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(item)), func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			if rel, err := filepath.Rel(dir, p); err == nil && rel != "." {
				dirs = append(dirs, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	under := func(rel string) bool { // the folder or one above it is protected
		for p := rel; p != ""; p = parent(p) {
			if protected[foldKey(p)] {
				return true
			}
		}
		return false
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if !under(dirs[i]) {
			rt.Remove(filepath.FromSlash(dirs[i])) // fails, harmlessly, while it still holds something
		}
	}
}

// RankSummary is how much of a space each rank holds.
func RankSummary(its []Item) map[string]float64 {
	m := map[string]float64{}
	for _, it := range its {
		m[it.Rank] += it.GB
	}
	return m
}
