// Package storage manages the disk space the human gives agents (Josh 11/20: "agents can manage the
// storage on my device that I allocate to them").
//
// The human sets an allotment: a folder (storage.root) and a size (storage.gb), and optionally a
// most any one project may hold (storage.projects.<name>). Inside it each project claims a space: a
// folder of its own with a size in GB. Agents claim, grow, shrink and release spaces themselves, and
// may delete anything inside their own space; they never write big files outside it. Claims can't
// add up to more than the allotment, and a claim's growth must fit on the drive beyond the free space
// kept for the human (reserve.disk_free_gb).
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Pool is the human's allotment, from firefighter.toml.
type Pool struct {
	Root    string             // folder holding every project's space
	GB      float64            // total the human gives agents (0: none)
	Caps    map[string]float64 // most one project may claim, when the human set one
	FloorGB func(path string) (freeGB, floorGB float64, ok bool)
}

// Grant is one project's claimed space.
type Grant struct {
	GB  float64   `json:"gb"`
	Why string    `json:"why,omitempty"`
	At  time.Time `json:"at"`
	// Root: the storage.root it was claimed under. When the human moves storage.root, the files stay
	// where they were: the space reports them as elsewhere instead of counting them.
	Root string `json:"root,omitempty"`
	// Labels: what agents said each file or folder in the space is for and how much it matters,
	// by its path inside the space ("/" separators).
	Labels map[string]Label `json:"labels,omitempty"`
}

type file struct {
	Grants map[string]Grant `json:"grants"`
}

// Space is one project's space as measured now.
type Space struct {
	Project string  `json:"project"`
	Path    string  `json:"path"`
	GB      float64 `json:"gb"`      // granted (0: a folder in the root nobody claimed)
	UsedGB  float64 `json:"used_gb"` // what its files hold
	Files   int     `json:"files"`
	Why     string  `json:"why,omitempty"`
	Over    bool    `json:"over,omitempty"` // holds more than it was granted
	// Elsewhere: the old root's folder, when the space was claimed under a storage.root since moved.
	Elsewhere string `json:"elsewhere,omitempty"`
	// RankGB: GB by rank (keep, useful, unranked, scratch), so anyone can see what may go first.
	RankGB map[string]float64 `json:"rank_gb,omitempty"`
}

// Report is the allotment and every space in it.
type Report struct {
	Root      string  `json:"root"`
	GB        float64 `json:"gb"`
	GrantedGB float64 `json:"granted_gb"`
	UsedGB    float64 `json:"used_gb"`
	DriveFree float64 `json:"drive_free_gb"`
	FloorGB   float64 `json:"drive_floor_gb"`
	Spaces    []Space `json:"spaces"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?$`)

var reserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\.|$)`)

// Name turns a project name (a folder or repo name, maybe with spaces) into the name of its space:
// letters, digits, '.', '_' and '-', no trailing dot, never a Windows device name. Jobs and spaces
// are matched through it, so "My Project" and its space "My-Project" are one project.
func Name(project string) string {
	b := []rune{}
	for _, r := range strings.TrimSpace(project) {
		if r < 128 && (r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b = append(b, r)
		} else {
			b = append(b, '-')
		}
	}
	n := strings.Trim(string(b), ".-")
	if len(n) > 64 {
		n = strings.TrimRight(n[:64], ".")
	}
	if n == "" {
		return ""
	}
	if reserved.MatchString(n) {
		n = "_" + n
	}
	if !nameRe.MatchString(n) {
		n = "p" + n
	}
	return n
}

// ErrNoAllotment: the human hasn't given agents any storage.
var ErrNoAllotment = errors.New("no storage allotted to agents: the human sets one with `firefighter config set storage.gb <GB>` (folder: storage.root)")

func path(home string) string { return filepath.Join(home, "storage.json") }

// Dir is a project's space.
func (p Pool) Dir(project string) (string, error) {
	if !nameRe.MatchString(project) || reserved.MatchString(project) {
		return "", fmt.Errorf("project %q: use letters, digits, '.', '_' or '-' (up to 64, not ending in '.', not a device name)", project)
	}
	if p.Root == "" || !filepath.IsAbs(p.Root) {
		return "", fmt.Errorf("storage.root %q must be an absolute folder", p.Root)
	}
	return filepath.Join(p.Root, project), nil
}

// lock serializes changes to storage.json across processes.
func lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	l := filepath.Join(home, "storage.lock")
	for start := time.Now(); ; {
		f, err := os.OpenFile(l, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			me := strconv.Itoa(os.Getpid())
			f.WriteString(me)
			f.Close()
			return func() {
				if b, _ := os.ReadFile(l); string(b) == me { // never another process's lock
					os.Remove(l)
				}
			}, nil
		}
		if fi, e := os.Stat(l); e == nil && time.Since(fi.ModTime()) > time.Second {
			b, _ := os.ReadFile(l)
			pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
			if (perr == nil && !sysinfo.Alive(pid)) || time.Since(fi.ModTime()) > 10*time.Minute {
				os.Remove(l) // left by a process that died holding it
				continue
			}
		}
		if time.Since(start) > 30*time.Second {
			return nil, fmt.Errorf("%s is held by another firefighter: try again", l)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func load(home string) (file, error) {
	f := file{Grants: map[string]Grant{}}
	b, err := os.ReadFile(path(home))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path(home), err)
	}
	if f.Grants == nil {
		f.Grants = map[string]Grant{}
	}
	return f, nil
}

func save(home string, f file) error {
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp := path(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(home))
}

// Grants reads the claimed spaces.
func Grants(home string) (map[string]Grant, error) {
	f, err := load(home)
	return f.Grants, err
}

// Measure adds up the files under dir. Links and junctions aren't followed.
func Measure(dir string) (gb float64, files int, err error) {
	var bytes int64
	err = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return nil // unreadable entries are skipped, not fatal
		}
		if e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				bytes += fi.Size()
				files++
			}
		}
		return nil
	})
	return float64(bytes) / (1 << 30), files, err
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// marker marks a folder as firefighter's storage root: only a marked root's folders are spaces that
// may be deleted, so pointing storage.root at a folder of the human's never exposes its files.
const marker = ".firefighter-storage"

// ensureRoot makes the root (new or empty) and marks it; an unmarked folder that already holds
// files is refused.
func (p Pool) ensureRoot() error {
	if _, err := os.Stat(filepath.Join(p.Root, marker)); err == nil {
		return nil
	}
	if es, err := os.ReadDir(p.Root); err == nil && len(es) > 0 {
		return fmt.Errorf("storage.root %s already holds other files: the human points storage.root at a new or empty folder", p.Root)
	}
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.Root, marker), []byte("Agents' storage spaces, one folder per project. Managed by `firefighter storage`.\n"), 0o600)
}

func (p Pool) marked() bool {
	_, err := os.Stat(filepath.Join(p.Root, marker))
	return err == nil
}

// canonical is the grant's own spelling of a project name (folders are case-insensitive on Windows
// and macOS: "Alpha" and "alpha" are one space).
func canonical(f file, project string) string {
	for name := range f.Grants {
		if strings.EqualFold(name, project) {
			return name
		}
	}
	return project
}

// capOf is the human's most for a project, whatever the spelling of its config key.
func (p Pool) capOf(project string) (float64, bool) {
	for name, c := range p.Caps {
		if strings.EqualFold(name, project) || strings.EqualFold(Name(name), project) {
			return c, true
		}
	}
	return 0, false
}

// Claim sets a project's space to gb (a new claim, or a bigger or smaller one) and returns its folder.
func (p Pool) Claim(home, project string, gb float64, why string) (string, error) {
	if p.GB <= 0 {
		return "", ErrNoAllotment
	}
	dir, err := p.Dir(project)
	if err != nil {
		return "", err
	}
	if gb <= 0 || math.IsNaN(gb) || math.IsInf(gb, 0) {
		return "", errors.New("--gb must be more than 0 (to give a space back: firefighter storage release)")
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
	if err := p.ensureRoot(); err != nil {
		return "", err
	}
	project = canonical(f, project)
	dir = filepath.Join(p.Root, project)
	if c, ok := p.capOf(project); ok && gb > c {
		return "", fmt.Errorf("%s may hold at most %.1f GB (storage.projects.%s, set by the human)", project, c, project)
	}
	others, list := 0.0, []string{}
	for name, g := range f.Grants {
		if name != project {
			others += g.GB
			list = append(list, fmt.Sprintf("%s %.1f", name, g.GB))
		}
	}
	sort.Strings(list)
	if left := p.GB - others; gb > left+1e-9 {
		held := "nothing else is claimed"
		if len(list) > 0 {
			held = "claimed: " + strings.Join(list, ", ") + " GB"
		}
		return "", fmt.Errorf("%.1f GB of the %.1f GB allotment is unclaimed (%s); claim at most %.1f GB, or ask the human for more (storage.gb)",
			math.Max(0, left), p.GB, held, math.Max(0, left))
	}
	used, _, _ := Measure(dir)
	if gb < used {
		return "", fmt.Errorf("%s's space already holds %.1f GB: delete files in %s first, then shrink it", project, used, dir)
	}
	// The drive must hold every space at its full size beyond the free space kept for the human. A
	// claim that shrinks (or stays) is always allowed: it is what a full drive needs.
	if prev, ok := f.Grants[project]; (!ok || gb > prev.GB) && p.FloorGB != nil {
		if free, floor, ok := p.FloorGB(firstExisting(dir)); ok {
			unused := gb - used
			for name, g := range f.Grants {
				if name != project {
					u, _, _ := Measure(filepath.Join(p.Root, name))
					unused += math.Max(0, g.GB-u)
				}
			}
			if room := free - floor; unused > room+1e-9 {
				return "", fmt.Errorf("the drive has %.1f GB free and keeps %.0f GB for the human; the claimed spaces' unused room would need %.1f GB of it: claim less, or free disk space",
					free, floor, unused)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	g := f.Grants[project]
	if g.At.IsZero() {
		g.At = time.Now()
	}
	g.GB = round1(gb)
	g.Root = p.Root
	if why != "" {
		g.Why = why
	}
	f.Grants[project] = g
	return dir, save(home, f)
}

// Release gives a project's space back. A space that still holds files is kept unless deleteFiles,
// which removes its folder and everything in it.
func (p Pool) Release(home, project string, deleteFiles bool) (freedGB float64, err error) {
	dir, err := p.Dir(project)
	if err != nil {
		return 0, err
	}
	unlock, err := lock(home)
	if err != nil {
		return 0, err
	}
	defer unlock()
	f, err := load(home)
	if err != nil {
		return 0, err
	}
	project = canonical(f, project)
	dir = filepath.Join(p.Root, project)
	g, granted := f.Grants[project]
	if granted && g.Root != "" && !samePath(g.Root, p.Root) {
		delete(f.Grants, project) // its files are in the old root, which firefighter no longer manages
		return 0, save(home, f)
	}
	used, files, _ := Measure(dir)
	if !granted && files == 0 {
		return 0, fmt.Errorf("%s has no space", project)
	}
	if files > 0 && deleteFiles {
		for _, it := range items(dir, g.Labels) {
			if it.Rank == Keep && it.Files > 0 {
				return 0, fmt.Errorf("%s holds %s, ranked keep (%s): move it out, or rank it none first, then release --delete", project, it.Path, it.For)
			}
		}
	}
	if files > 0 {
		if !p.marked() {
			return 0, fmt.Errorf("%s isn't a storage root firefighter made (no %s in it): nothing deleted", p.Root, marker)
		}
		if !deleteFiles {
			return 0, fmt.Errorf("%s's space still holds %.1f GB in %d file(s) (%s): move what you keep elsewhere and pass --delete, or delete them yourself first",
				project, used, files, dir)
		}
		if err := removeSpace(p.Root, dir); err != nil {
			return 0, err
		}
	} else {
		os.Remove(dir)
	}
	delete(f.Grants, project)
	return used, save(home, f)
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// removeSpace deletes one space's folder, only when it sits directly inside the root.
func removeSpace(root, dir string) error {
	r, err1 := filepath.Abs(root)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil || !strings.EqualFold(filepath.Dir(d), r) || filepath.Dir(r) == r {
		return fmt.Errorf("refusing to delete %s: not a space inside %s", dir, root)
	}
	return os.RemoveAll(d)
}

// Look measures every space, claimed or not, in the root.
func (p Pool) Look(home string) (Report, error) {
	r := Report{Root: p.Root, GB: p.GB}
	grants, err := Grants(home)
	if err != nil {
		return r, err
	}
	seen := map[string]bool{}
	add := func(name string, g Grant, claimed bool) {
		dir := filepath.Join(p.Root, name)
		its := items(dir, g.Labels)
		used, files := 0.0, 0
		for _, it := range its {
			used += it.GB
			files += it.Files
		}
		s := Space{Project: name, Path: dir, GB: g.GB, UsedGB: round1(used), Files: files, Why: g.Why, RankGB: map[string]float64{}}
		for r, x := range RankSummary(its) {
			s.RankGB[r] = round1(x)
		}
		s.Over = used > g.GB+0.05 || (!claimed && used > 0.05)
		r.Spaces = append(r.Spaces, s)
		r.GrantedGB += g.GB
		r.UsedGB += used
	}
	for name, g := range grants {
		if g.Root != "" && !samePath(g.Root, p.Root) {
			// claimed under a storage.root the human has since moved: its files weren't moved
			r.Spaces = append(r.Spaces, Space{Project: name, Path: filepath.Join(p.Root, name), GB: g.GB, Why: g.Why,
				Elsewhere: filepath.Join(g.Root, name)})
			r.GrantedGB += g.GB
			continue
		}
		seen[strings.ToLower(name)] = true
		add(name, g, true)
	}
	if es, err := os.ReadDir(p.Root); err == nil && p.marked() {
		for _, e := range es {
			if e.IsDir() && !seen[strings.ToLower(e.Name())] {
				add(e.Name(), Grant{}, false)
			}
		}
	}
	sort.Slice(r.Spaces, func(a, b int) bool { return r.Spaces[a].Project < r.Spaces[b].Project })
	r.GrantedGB, r.UsedGB = round1(r.GrantedGB), round1(r.UsedGB)
	if p.FloorGB != nil {
		if free, floor, ok := p.FloorGB(firstExisting(p.Root)); ok {
			r.DriveFree, r.FloorGB = round1(free), floor
		}
	}
	return r, nil
}

// firstExisting is path or its nearest folder that exists (for finding its drive).
func firstExisting(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil || filepath.Dir(p) == p {
			return p
		}
	}
}

// FromConfig is the allotment firefighter.toml sets.
func FromConfig(c config.Config) Pool {
	return Pool{Root: c.StorageRoot(), GB: c.Storage.GB, Caps: c.Storage.Projects,
		FloorGB: func(path string) (float64, float64, bool) {
			dk, ok := sysinfo.DiskOf(path)
			if !ok {
				return 0, 0, false
			}
			return dk.FreeGB, math.Min(c.Reserve.DiskFreeGB, dk.TotalGB/10), true
		}}
}
