package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// History is what firefighter learned about one command in one project: how its runtime compares to
// what agents declare, and its peak VRAM and RAM. Admission and ordering use it instead of trusting
// declarations.
type History struct {
	Ratios   []float64 `json:"ratios"` // last 5 actual/declared runtimes
	PeakVRAM float64   `json:"peak_vram_gb"`
	PeakRAM  float64   `json:"peak_ram_gb"`
	Runs     int       `json:"runs"`
	Last     time.Time `json:"last,omitempty"`    // last run, for pruning
	Name     string    `json:"name,omitempty"`    // the job's name at its last run, for `firefighter forecast`
	GPUJob   bool      `json:"gpu_job,omitempty"` // it ran as a GPU job
	// One sample per finished run, older runs fading (usage.AddRun): peaks for what a job must not
	// run out of, means for what it can be throttled on.
	RAM  *usage.Hist `json:"ram,omitempty"`      // peak RAM, GB
	VRAM *usage.Hist `json:"vram,omitempty"`     // peak VRAM, GB
	CPU  *usage.Hist `json:"cpu,omitempty"`      // mean CPUs busy
	GPU  *usage.Hist `json:"gpu_util,omitempty"` // mean GPU utilization, %
	Disk *usage.Hist `json:"disk,omitempty"`     // GB written
	Net  *usage.Hist `json:"net,omitempty"`      // mean Mbps (network jobs)
	Mins *usage.Hist `json:"minutes,omitempty"`  // minutes worked
}

// HumanUse is what the human (and every program that isn't a firefighter job) has been seen to use
// in one user state: the basis of that state's learned reserve. Samples are taken every 5 s and
// fade with a 7-day half-life.
type HumanUse struct {
	CPU      *usage.Hist `json:"cpu"`       // CPUs busy
	RAMRise  *usage.Hist `json:"ram_rise"`  // GB their RAM use rose within 5 minutes
	VRAMRise *usage.Hist `json:"vram_rise"` // GB their VRAM use rose within 5 minutes
	Net      *usage.Hist `json:"net"`       // Mbps, while no network job runs
}

// Receipt is what an agent gets back when its job ends.
type Receipt struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Name     string    `json:"name"`
	Exit     int       `json:"exit"`
	Outcome  string    `json:"outcome"` // ok | failed | evicted | cancelled | lost | ended | killed (adopted work)
	Minutes  float64   `json:"minutes"`
	Declared float64   `json:"declared_minutes"`
	Waited   float64   `json:"waited_minutes"`
	Charged  float64   `json:"charged"`
	PeakVRAM float64   `json:"peak_vram_gb"`
	PeakRAM  float64   `json:"peak_ram_gb"`
	Log      string    `json:"log,omitempty"`
	Ended    time.Time `json:"ended"`
	Requeue  bool      `json:"requeue"`
	Reason   string    `json:"reason,omitempty"` // why firefighter stopped it (evicted/killed jobs)
	// Held: times the guard suspended it to free RAM or disk for the human (it resumed each time
	// where it stopped), for HeldMinutes in all. FreedRAM: the RAM that gave back, or, for a job
	// stopped for RAM, what stopping it freed.
	Held        int     `json:"held,omitempty"`
	HeldMinutes float64 `json:"held_minutes,omitempty"`
	FreedRAM    float64 `json:"freed_ram_gb,omitempty"`
	Client      int     `json:"client,omitempty"`    // the client that ran it: only it may reuse the id
	ClientAt    int64   `json:"client_at,omitempty"` // its start time (a reused pid isn't the same client)
}

// OpenJob is a running job's client, kept so that after a daemon restart only jobs this guard
// really granted can re-register as running (anything else is adopted: spot band, 1.5x).
type OpenJob struct {
	Client   int       `json:"client"`
	ClientAt int64     `json:"client_at"`
	Seen     time.Time `json:"seen"`
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
	Owner   int       `json:"owner,omitempty"`    // agent session that announced it; -1: the human
	OwnerAt int64     `json:"owner_at,omitempty"` // its start time (a reused pid isn't the same session)
}

type saved struct {
	Accounts map[string]*policy.Account `json:"accounts"`
	History  map[string]*History        `json:"history"`
	Receipts map[string]*Receipt        `json:"receipts"`
	Plans    []*Plan                    `json:"plans"`
	// Frozen: processes the daemon suspended (pid -> start time), so a daemon that dies mid-game
	// can thaw them on its next start even when no client re-registers them (adopted work).
	Frozen map[int]int64 `json:"frozen,omitempty"`
	// Open: running jobs by id, so their clients can re-register them after a restart.
	Open map[string]OpenJob `json:"open,omitempty"`
	// Newcomers: the budget never-seen projects draw from, refilled daily (guard.new_project_minutes).
	Newcomers   float64 `json:"newcomers"`
	NetLinkMbps float64 `json:"net_link_mbps,omitempty"` // the network link's learned size, kept across restarts
	// Human: the human's use per user state (away, present, active, game), for the learned reserve.
	Human       map[string]*HumanUse `json:"human,omitempty"`
	NewcomersAt time.Time            `json:"newcomers_at"`
	// Occupancy is how taken the GPU usually is, per hour of the week (0-1), for quotes.
	Occupancy  []float64 `json:"occupancy"`
	OccSamples []float64 `json:"occupancy_hours"` // hours of observation behind each slot
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
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}
