// Package config holds every knob firefighter has. Defaults are the researched policy; a preset
// shifts them for a kind of machine use; firefighter.toml overrides any single value. Agents change
// it with `firefighter config set <dotted.key> <value>` or the firefighter_config MCP tool.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Reserve is what the human keeps in one user state. Jobs never get it.
type Reserve struct {
	CPUs     int     `toml:"cpus"`      // logical CPUs kept free of jobs
	RAMPct   float64 `toml:"ram_pct"`   // share of RAM kept available
	VRAMGB   float64 `toml:"vram_gb"`   // VRAM kept free for non-job use
	PauseGPU bool    `toml:"pause_gpu"` // cooperative pause of running GPU jobs, no new GPU grants
}

type Config struct {
	Preset string `toml:"preset"` // balanced | gamer | throughput | fair

	Order struct {
		Policy         string  `toml:"policy"`           // las | stride | lottery | drf | fifo | srpt
		HalfLifeHours  float64 `toml:"half_life_hours"`  // decay of attained service (las, drf)
		MaxWaitMinutes float64 `toml:"max_wait_minutes"` // anything waiting this long goes next
	} `toml:"order"`

	Pricing struct {
		Policy         string  `toml:"policy"`           // priced | free
		MaxLeadMinutes float64 `toml:"max_lead_minutes"` // most queue lead a bid of 10 can buy
	} `toml:"pricing"`

	Budget struct {
		Policy       string             `toml:"policy"`        // karma | daily | none
		DailyMinutes float64            `toml:"daily_minutes"` // machine-minutes: all of the GPU (or of any one resource) for a minute
		BankDays     float64            `toml:"bank_days"`     // karma: unused allowance saved up to this many days
		Projects     map[string]float64 `toml:"projects"`      // per-project daily minutes
		Weights      map[string]float64 `toml:"weights"`       // per-project share weight (default 1)
	} `toml:"budget"`

	GPU struct {
		MaxJobs       int     `toml:"max_jobs"`        // GPU jobs at once
		PackBelowUtil float64 `toml:"pack_below_util"` // a second GPU job joins only below this GPU %
		VRAMMargin    float64 `toml:"vram_margin"`     // learned peak VRAM is multiplied by this
		DefaultVRAMGB float64 `toml:"default_vram_gb"` // need assumed for a GPU job with no history
	} `toml:"gpu"`

	Reserve struct {
		Away              Reserve  `toml:"away"`
		Present           Reserve  `toml:"present"`
		Active            Reserve  `toml:"active"`
		Game              Reserve  `toml:"game"`
		AwaySeconds       float64  `toml:"away_seconds"`   // no input this long = away
		ActiveSeconds     float64  `toml:"active_seconds"` // input this recent = active
		ForeignGPUPct     float64  `toml:"foreign_gpu_pct"`
		CPUShrinkSeconds  float64  `toml:"cpu_shrink_seconds"`  // reserve gives back 1 CPU per this
		RAMShrinkSeconds  float64  `toml:"ram_shrink_seconds"`  // RAM reserve holds this long before shrinking
		VRAMShrinkSeconds float64  `toml:"vram_shrink_seconds"` // VRAM reserve holds this long
		GameCoolSeconds   float64  `toml:"game_cool_seconds"`   // game state holds this long after it ends
		ExclusiveApps     []string `toml:"exclusive_apps"`      // process names that count as a game
		IgnoreGPUApps     []string `toml:"ignore_gpu_apps"`     // compositors and harvesters: GPU use that isn't the human
	} `toml:"reserve"`

	Ladder struct {
		PauseAckSeconds   float64 `toml:"pause_ack_seconds"` // cooperative pause ignored this long -> suspend
		SuspendMaxSeconds float64 `toml:"suspend_max_seconds"`
		PressureSeconds   float64 `toml:"pressure_seconds"` // RAM/VRAM under reserve this long -> checkpoint
		GraceSeconds      float64 `toml:"grace_seconds"`    // checkpoint request -> kill
		KillBelowRAMPct   float64 `toml:"kill_below_ram_pct"`
		MinRuntimeMinutes float64 `toml:"min_runtime_minutes"` // no job-vs-job preemption before this
		OverrunFactor     float64 `toml:"overrun_factor"`      // past lease x this -> spot band, double charge
	} `toml:"ladder"`

	Guard struct { // fallbacks for agents that skip firefighter, by accident or on purpose
		AutoAdopt    bool     `toml:"auto_adopt"`    // govern heavy processes agents started outside firefighter
		AgentParents []string `toml:"agent_parents"` // process names whose descendants count as agent work
		HeavyCPUs    float64  `toml:"heavy_cpus"`    // sustained CPU use that counts as heavy
		HeavyRAMGB   float64  `toml:"heavy_ram_gb"`
		HeavySeconds float64  `toml:"heavy_seconds"` // how long before it's adopted
		// AgentsCanConfigure lets the firefighter_config MCP tool change settings. Off: agents can
		// read the policy but only the human (CLI) changes it.
		AgentsCanConfigure bool `toml:"agents_can_configure"`
	} `toml:"guard"`

	Harvester struct { // band 4: something that only runs when nothing else wants the GPU (Salad, BOINC, ...)
		Start       string  `toml:"start"`
		Stop        string  `toml:"stop"`
		IdleSeconds float64 `toml:"idle_seconds"`
	} `toml:"harvester"`
}

func Defaults() Config {
	var c Config
	c.Preset = "balanced"
	c.Order.Policy = "las"
	c.Order.HalfLifeHours = 12
	c.Order.MaxWaitMinutes = 45
	c.Pricing.Policy = "priced"
	c.Pricing.MaxLeadMinutes = 30
	c.Budget.Policy = "karma"
	c.Budget.DailyMinutes = 240
	c.Budget.BankDays = 2
	c.Budget.Projects = map[string]float64{}
	c.Budget.Weights = map[string]float64{}
	c.GPU.MaxJobs = 2
	c.GPU.PackBelowUtil = 70
	c.GPU.VRAMMargin = 1.2
	c.GPU.DefaultVRAMGB = 4
	c.Reserve.Away = Reserve{CPUs: 2, RAMPct: 5, VRAMGB: 1}
	c.Reserve.Present = Reserve{CPUs: 4, RAMPct: 8, VRAMGB: 1.5}
	c.Reserve.Active = Reserve{CPUs: 6, RAMPct: 10, VRAMGB: 2}
	c.Reserve.Game = Reserve{CPUs: 8, RAMPct: 15, VRAMGB: 4, PauseGPU: true}
	c.Reserve.AwaySeconds = 180
	c.Reserve.ActiveSeconds = 10
	c.Reserve.ForeignGPUPct = 20
	c.Reserve.CPUShrinkSeconds = 15
	c.Reserve.RAMShrinkSeconds = 60
	c.Reserve.VRAMShrinkSeconds = 120
	c.Reserve.GameCoolSeconds = 30
	c.Reserve.IgnoreGPUApps = []string{"dwm.exe", "csrss.exe", "explorer.exe", "windowserver", "xorg", "gnome-shell", "kwin_wayland", "kwin_x11", "firefighter.exe", "firefighter"}
	c.Ladder.PauseAckSeconds = 10
	c.Ladder.SuspendMaxSeconds = 60
	c.Ladder.PressureSeconds = 20
	c.Ladder.GraceSeconds = 60
	c.Ladder.KillBelowRAMPct = 3
	c.Ladder.MinRuntimeMinutes = 10
	c.Ladder.OverrunFactor = 1.25
	c.Harvester.IdleSeconds = 180
	c.Guard.AutoAdopt = true
	// Agent CLIs only: IDEs (Cursor, Windsurf, VS Code) also host the human's own terminals.
	c.Guard.AgentParents = []string{"claude", "codex", "gemini", "aider", "goose", "opencode", "cline"}
	c.Guard.HeavyCPUs = 2
	c.Guard.HeavyRAMGB = 4
	c.Guard.HeavySeconds = 60
	return c
}

// Presets are applied on top of the defaults, before the user's own overrides.
var Presets = map[string]func(*Config){
	"balanced": func(*Config) {},
	"gamer": func(c *Config) { // the PC is mostly for play: bigger reserves, one GPU job, nothing during games
		c.GPU.MaxJobs = 1
		c.Reserve.Present = Reserve{CPUs: 6, RAMPct: 12, VRAMGB: 3}
		c.Reserve.Active = Reserve{CPUs: 8, RAMPct: 15, VRAMGB: 4}
		c.Reserve.Game = Reserve{CPUs: 10, RAMPct: 25, VRAMGB: 8, PauseGPU: true}
	},
	"throughput": func(c *Config) { // a build box: small reserves, pack the GPU
		c.GPU.MaxJobs = 3
		c.GPU.PackBelowUtil = 85
		c.Reserve.Present = Reserve{CPUs: 2, RAMPct: 5, VRAMGB: 1}
		c.Reserve.Active = Reserve{CPUs: 3, RAMPct: 6, VRAMGB: 1}
	},
	"fair": func(c *Config) { // shared lab machine: strict multi-resource fairness, no paid urgency
		c.Order.Policy = "drf"
		c.Pricing.Policy = "free"
		c.Budget.Policy = "daily"
	},
}

var OrderPolicies = []string{"las", "stride", "lottery", "drf", "fifo", "srpt"}

// Home is the state folder: FIREFIGHTER_HOME, else the per-user local data folder.
func Home() string {
	if h := os.Getenv("FIREFIGHTER_HOME"); h != "" {
		return h
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "firefighter")
		}
	case "darwin":
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "firefighter")
		}
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "firefighter")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "firefighter")
}

func Path() string { return filepath.Join(Home(), "firefighter.toml") }

// Load reads defaults, then the preset named in the file, then the file itself.
func Load() (Config, error) {
	raw := map[string]any{}
	if b, err := os.ReadFile(Path()); err == nil {
		if _, err := toml.Decode(string(b), &raw); err != nil {
			return Defaults(), fmt.Errorf("%s: %w", Path(), err)
		}
	}
	return build(raw)
}

func build(raw map[string]any) (Config, error) {
	c := Defaults()
	if p, ok := raw["preset"].(string); ok {
		apply, ok := Presets[p]
		if !ok {
			return c, fmt.Errorf("unknown preset %q (have %s)", p, strings.Join(keys(Presets), ", "))
		}
		apply(&c)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return c, err
	}
	if _, err := toml.Decode(buf.String(), &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !contains(OrderPolicies, c.Order.Policy) {
		return fmt.Errorf("order.policy %q: use one of %s", c.Order.Policy, strings.Join(OrderPolicies, ", "))
	}
	if !contains([]string{"priced", "free"}, c.Pricing.Policy) {
		return fmt.Errorf("pricing.policy %q: use priced or free", c.Pricing.Policy)
	}
	if !contains([]string{"karma", "daily", "none"}, c.Budget.Policy) {
		return fmt.Errorf("budget.policy %q: use karma, daily or none", c.Budget.Policy)
	}
	if c.GPU.MaxJobs < 1 {
		return fmt.Errorf("gpu.max_jobs must be at least 1")
	}
	return nil
}

// Set changes one dotted key in firefighter.toml ("order.policy", "budget.projects.myrepo",
// "reserve.game.cpus"), checks the result, and writes the file. Values parse as bool, number,
// a [list] or a string.
func Set(key, value string) error {
	raw := map[string]any{}
	if b, err := os.ReadFile(Path()); err == nil {
		if _, err := toml.Decode(string(b), &raw); err != nil {
			return err
		}
	}
	parts := strings.Split(key, ".")
	node := raw
	for _, p := range parts[:len(parts)-1] {
		next, ok := node[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[p] = next
		}
		node = next
	}
	node[parts[len(parts)-1]] = parse(value)
	if _, err := build(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# firefighter settings. Every key: `firefighter config show`. Change one: `firefighter config set <key> <value>`.\n")
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return err
	}
	return os.WriteFile(Path(), buf.Bytes(), 0o644)
}

// Unset removes a key so its default (or preset value) applies again.
func Unset(key string) error {
	raw := map[string]any{}
	b, err := os.ReadFile(Path())
	if err != nil {
		return nil
	}
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return err
	}
	parts := strings.Split(key, ".")
	node := raw
	for _, p := range parts[:len(parts)-1] {
		next, ok := node[p].(map[string]any)
		if !ok {
			return nil
		}
		node = next
	}
	delete(node, parts[len(parts)-1])
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return err
	}
	return os.WriteFile(Path(), buf.Bytes(), 0o644)
}

// Show renders the effective config as TOML.
func (c Config) Show() string {
	var buf bytes.Buffer
	_ = toml.NewEncoder(&buf).Encode(c)
	return buf.String()
}

func parse(v string) any {
	if v == "true" || v == "false" {
		return v == "true"
	}
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		var out []any
		for _, s := range strings.Split(strings.Trim(v, "[]"), ",") {
			if s = strings.Trim(strings.TrimSpace(s), `"'`); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReserveFor returns the reserve for a user state name.
func (c Config) ReserveFor(state string) Reserve {
	switch state {
	case "away":
		return c.Reserve.Away
	case "active":
		return c.Reserve.Active
	case "game":
		return c.Reserve.Game
	}
	return c.Reserve.Present
}

func (c Config) Weight(project string) float64 {
	if w, ok := c.Budget.Weights[project]; ok && w > 0 {
		return w
	}
	return 1
}

func (c Config) Allowance(project string) float64 {
	if m, ok := c.Budget.Projects[project]; ok {
		return m
	}
	return c.Budget.DailyMinutes
}
