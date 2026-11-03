package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
)

// History is what firefighter learned about one command in one project: how its runtime compares to
// what agents declare, and its peak VRAM and RAM. Admission and ordering use it instead of trusting
// declarations.
type History struct {
	Ratios   []float64 `json:"ratios"` // last 5 actual/declared runtimes
	PeakVRAM float64   `json:"peak_vram_gb"`
	PeakRAM  float64   `json:"peak_ram_gb"`
	Runs     int       `json:"runs"`
}

// Receipt is what an agent gets back when its job ends.
type Receipt struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Name     string    `json:"name"`
	Exit     int       `json:"exit"`
	Outcome  string    `json:"outcome"` // ok | failed | evicted | cancelled | lost
	Minutes  float64   `json:"minutes"`
	Declared float64   `json:"declared_minutes"`
	Waited   float64   `json:"waited_minutes"`
	Charged  float64   `json:"charged"`
	PeakVRAM float64   `json:"peak_vram_gb"`
	PeakRAM  float64   `json:"peak_ram_gb"`
	Log      string    `json:"log,omitempty"`
	Ended    time.Time `json:"ended"`
	Requeue  bool      `json:"requeue"`
}

// Plan is an announcement of heavy work coming later; it reserves nothing, it tells others.
type Plan struct {
	ID      string    `json:"id"`
	Project string    `json:"project"`
	What    string    `json:"what"`
	When    string    `json:"when"`
	Minutes float64   `json:"minutes"`
	GPUPct  float64   `json:"gpu_pct"`
	VRAMGB  float64   `json:"vram_gb"`
	RAMGB   float64   `json:"ram_gb"`
	Added   time.Time `json:"added"`
}

type saved struct {
	Accounts map[string]*policy.Account `json:"accounts"`
	History  map[string]*History        `json:"history"`
	Receipts map[string]*Receipt        `json:"receipts"`
	Plans    []*Plan                    `json:"plans"`
	// Occupancy is how taken the GPU usually is, per hour of the week (0-1), for quotes.
	Occupancy []float64 `json:"occupancy"`
}

func statePath() string { return filepath.Join(config.Home(), "state.json") }

func load() saved {
	s := saved{Accounts: map[string]*policy.Account{}, History: map[string]*History{}, Receipts: map[string]*Receipt{}}
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Accounts == nil {
		s.Accounts = map[string]*policy.Account{}
	}
	if s.History == nil {
		s.History = map[string]*History{}
	}
	if s.Receipts == nil {
		s.Receipts = map[string]*Receipt{}
	}
	return s
}

func (s saved) write() error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}
