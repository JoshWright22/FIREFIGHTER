package daemon

import (
	"bytes"
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
	// Failure: why a job that ended on its own failed, whether running it again can help, and what
	// to change (from its exit code and the tail of its output; OpenPAI's job exit spec).
	Failure *policy.Failure `json:"failure,omitempty"`
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
		if err := json.Unmarshal(b, &s); err != nil {
			// Keep the damaged file for a look and start clean, instead of running on half of it.
			bad := statePath() + ".bad-" + time.Now().Format("20060102-150405")
			logf("state.json is damaged (%v); moved to %s, starting with empty accounts and history", err, bad)
			_ = os.Rename(statePath(), bad)
			s = salvage(b) // the processes the dead guard froze must still be thawed
		}
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

// saveState writes state.json now and says so when it can't (a full disk would otherwise lose
// accounts and receipts without a word).
func (d *Daemon) saveState() {
	if err := d.st.write(); err != nil {
		logf("couldn't save state.json: %v", err)
	}
}

// salvage reads the top-level entries of a damaged state.json up to the damage, and keeps only the
// processes the guard froze and the jobs open then: losing those would leave a game-time freeze in
// place for good (review round 6). Accounts and history start over.
func salvage(b []byte) saved {
	var s saved
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return s
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		switch k {
		case "frozen":
			_ = json.Unmarshal(raw, &s.Frozen)
		case "open":
			_ = json.Unmarshal(raw, &s.Open)
		}
	}
	return s
}
