package daemon

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/usage"
)

// Learning what the human uses, and sizing their reserve from it.
//
// The configured reserve per user state (reserve.active.cpus, ram_pct, vram_gb, ...) is a guess
// made before the machine was ever watched. Every 5 s the guard records what the human and every
// program that isn't a firefighter job are using, per user state, in decaying histograms
// (usage.Hist: Autopilot's and VPA's data structure). After a day of history in a state, that
// state's reserve follows the history instead of the guess, within half to three times the
// configured value:
//
//   - CPUs: the load-adjusted 95th percentile of the CPUs they keep busy (Autopilot Eq. 3: the
//     level that covers 95% of their work, not of their time), plus 2 idle cores (PerfIso's buffer).
//   - RAM and VRAM: the 95th percentile of how much their use rises within 5 minutes, plus 15%:
//     the free memory that has to be there now so the rise never finds the machine full.
//
// The instant rules stay on top (the CPU reserve also follows what is in use this second; a game
// holds the GPU): learning sets the baseline, it never slows the reaction.

const (
	humanEvery    = 5 * time.Second
	humanHalfLife = 7 * 24 * time.Hour // a week: last week's habits count half
	riseHorizon   = 5 * time.Minute
	humanPct      = 0.95
)

func newHumanUse() *HumanUse {
	return &HumanUse{CPU: usage.NewHist(0.05), RAMRise: usage.NewHist(0.05), VRAMRise: usage.NewHist(0.05), Net: usage.NewHist(0.1)}
}

func (d *Daemon) humanUse(state string) *HumanUse {
	if d.st.Human == nil {
		d.st.Human = map[string]*HumanUse{}
	}
	h := d.st.Human[state]
	if h == nil || h.CPU == nil || h.RAMRise == nil || h.VRAMRise == nil || h.Net == nil {
		h = newHumanUse()
		d.st.Human[state] = h
	}
	return h
}

// observeHuman samples the human's use of every resource (everything that isn't a job's).
func (d *Daemon) observeHuman(now time.Time, dt time.Duration) {
	if dt <= 0 || now.Sub(d.humanAt) < humanEvery || d.snap.RAMTotalGB <= 0 {
		return // (dt 0: the machine slept, or the first tick)
	}
	d.humanAt = now
	jobRAM, jobVRAM, netJob := 0.0, 0.0, false
	for _, j := range d.running() {
		jobRAM += j.ramGB
		jobVRAM += j.vramGB
		netJob = netJob || j.Net
	}
	h := d.humanUse(d.user)
	for _, x := range []*usage.Hist{h.CPU, h.RAMRise, h.VRAMRise, h.Net} {
		x.Age(now, humanHalfLife)
	}
	// A reading taken with every CPU busy says what the human got, not what they wanted
	// (SmartHarvest drops samples taken while the primary was saturated): it isn't learned from.
	if d.snap.CPUBusy < 0.95*float64(max(1, d.snap.CPUs)) {
		h.CPU.Add(d.busyUser, 1, now)
	}
	if !netJob && now.Sub(d.netQuietFrom) >= netMeasure {
		h.Net.Add(d.netHuman, 1, now)
	}
	// The local Linux VM (WSL2, Docker) is left out whole: most of what it holds is file cache it
	// gives back on request, and how much is only known while RAM is tight, so counting it would
	// record a build's cache as the human's need. The RAM ladder covers it instead (reclaim first).
	ram := math.Max(0, d.snap.RAMTotalGB-d.snap.RAMAvailGB-jobRAM-d.vm.HostGB)
	rises, states := d.ramRise.Push(now, ram, d.user, riseHorizon)
	for i, r := range rises {
		d.humanUse(states[i]).RAMRise.Add(r, 1, now)
	}
	if !d.gpuDegraded && len(d.snap.GPUs) > 0 {
		_, used := d.snap.VRAMTotal()
		rises, states = d.vramRise.Push(now, math.Max(0, used-jobVRAM), d.user, riseHorizon)
		for i, r := range rises {
			d.humanUse(states[i]).VRAMRise.Add(r, 1, now)
		}
	}
}

// learnedReserve is a state's reserve as the human's history sizes it (see the top of this file);
// the configured reserve until that state has a day of history, or with reserve.learn off.
func (d *Daemon) learnedReserve(state string, now time.Time) config.Reserve {
	base := d.cfg.ReserveFor(state)
	h := d.st.Human[state]
	if !d.cfg.Reserve.Learn || h == nil || h.CPU == nil {
		return base
	}
	r := base
	days := h.CPU.Days(now, humanEvery)
	cpus := math.Ceil(h.CPU.LoadPercentile(humanPct)) + 2
	r.CPUs = int(math.Round(usage.Reserve(float64(base.CPUs), cpus, days, 0)))
	if h.RAMRise != nil && d.snap.RAMTotalGB > 0 {
		pct := h.RAMRise.Percentile(humanPct) / d.snap.RAMTotalGB * 100
		r.RAMPct = math.Round(usage.Reserve(base.RAMPct, pct, h.RAMRise.Days(now, humanEvery), usage.SafetyMargin)*10) / 10
	}
	if h.VRAMRise != nil {
		r.VRAMGB = math.Round(usage.Reserve(base.VRAMGB, h.VRAMRise.Percentile(humanPct), h.VRAMRise.Days(now, humanEvery), usage.SafetyMargin)*10) / 10
	}
	return r
}

// withLearnedReserve is the settings with the current state's reserve replaced by the learned one.
func (d *Daemon) withLearnedReserve(c config.Config, now time.Time) config.Config {
	r := d.learnedReserve(d.user, now)
	switch d.user {
	case "away":
		c.Reserve.Away = r
	case "active":
		c.Reserve.Active = r
	case "game":
		c.Reserve.Game = r
	default:
		c.Reserve.Present = r
	}
	return c
}

// projectPrior is the cold-start need for a command never run before: the median of what the
// project's other commands of the same kind (GPU or CPU) need, when at least two are known (the
// lower of the two middle ones: one heavy sibling must not make every new command look heavy; its
// own first run, its declaration and the RAM ladder cover the rest).
// 3Sigma (EuroSys 2018) predicts from several features of a job (user, program) and uses whichever
// has history; here the levels are the command, then its project.
func (d *Daemon) projectPrior(project string, gpu bool) (ramGB, vramGB float64, ok bool) {
	var rams, vrams []float64
	for key, h := range d.st.History {
		if h.GPUJob != gpu || h.RAM == nil || h.RAM.N == 0 || len(key) <= len(project) || key[:len(project)+1] != project+"|" {
			continue
		}
		rams = append(rams, usage.PeakNeed(h.RAM, 0, 0))
		vrams = append(vrams, usage.PeakNeed(h.VRAM, 0, 0))
	}
	if len(rams) < 2 {
		return 0, 0, false
	}
	median := func(v []float64) float64 {
		sort.Float64s(v)
		return v[(len(v)-1)/2]
	}
	return median(rams), median(vrams), true
}

// jobCPUs is the CPUs the running and granted jobs are expected to keep busy: for each, the larger
// of what it uses now and its past runs' mean.
func (d *Daemon) jobCPUs() float64 {
	sum := 0.0
	for _, j := range d.jobs {
		if j.State == "running" || j.State == "granted" {
			sum += math.Max(j.cpuNow, j.NeedCPU)
		}
	}
	return sum
}

// cpuRoom is how many CPUs jobs may plan on: those outside the human's reserve, oversubscribed by a
// quarter (Resource Central's MAX_OVERSUB = 125%, SOSP 2017, Algorithm 1): a job's mean is not its
// peak, and CPU is the resource a job can be throttled on without losing work.
func (d *Daemon) cpuRoom() float64 {
	return math.Max(1, float64(d.snap.CPUs)-math.Round(d.res.CPUs)) * 1.25
}

// ---- forecast, for status and `firefighter usage` -------------------------------------------------

// Forecast is what firefighter expects the human and each known command to use.
type Forecast struct {
	Human    []HumanForecast   `json:"human"`
	Commands []CommandForecast `json:"commands"`
}

// HumanForecast is one user state: the configured reserve, the reserve in force (learned once there
// is a day of history), and the measurements behind it.
type HumanForecast struct {
	State      string  `json:"state"`
	Now        bool    `json:"now,omitempty"` // the human is in this state
	Days       float64 `json:"days"`          // days of history in this state
	Learned    bool    `json:"learned"`       // the reserve below comes from history, not the configuration
	CPUs       int     `json:"cpus"`
	RAMPct     float64 `json:"ram_pct"`
	VRAMGB     float64 `json:"vram_gb"`
	SetCPUs    int     `json:"configured_cpus"`
	SetRAMPct  float64 `json:"configured_ram_pct"`
	SetVRAMGB  float64 `json:"configured_vram_gb"`
	CPUP95     float64 `json:"cpu_p95"`          // CPUs busy, load-adjusted P95
	CPUMean    float64 `json:"cpu_mean"`         // CPUs busy on average
	RAMRiseGB  float64 `json:"ram_rise_p95_gb"`  // P95 rise in RAM use within 5 minutes
	VRAMRiseGB float64 `json:"vram_rise_p95_gb"` // P95 rise in VRAM use within 5 minutes
	NetMbps    float64 `json:"net_p95_mbps"`     // P95 network use
}

// CommandForecast is one command in one project: what its next run is expected to need.
type CommandForecast struct {
	Project string  `json:"project"`
	Name    string  `json:"name"`
	Runs    int     `json:"runs"`
	RAMGB   float64 `json:"ram_gb"`
	VRAMGB  float64 `json:"vram_gb"`
	CPUs    float64 `json:"cpus"`
	GPUPct  float64 `json:"gpu_pct"`
	DiskGB  float64 `json:"disk_gb"`
	NetMbps float64 `json:"net_mbps"`
	Minutes float64 `json:"minutes"`
	Warm    bool    `json:"warm"` // enough runs that history is trusted over the declaration
}

func (d *Daemon) forecast(now time.Time) *Forecast {
	f := &Forecast{}
	for _, state := range []string{"away", "present", "active", "game"} {
		set, r := d.cfg.ReserveFor(state), d.learnedReserve(state, now)
		hf := HumanForecast{State: state, Now: state == d.user, CPUs: r.CPUs, RAMPct: r.RAMPct, VRAMGB: r.VRAMGB,
			SetCPUs: set.CPUs, SetRAMPct: set.RAMPct, SetVRAMGB: set.VRAMGB}
		if h := d.st.Human[state]; h != nil && h.CPU != nil {
			hf.Days = math.Round(h.CPU.Days(now, humanEvery)*100) / 100
			hf.Learned = d.cfg.Reserve.Learn && hf.Days >= 1
			hf.CPUP95, hf.CPUMean = round1(h.CPU.LoadPercentile(humanPct)), round1(h.CPU.Mean())
			hf.RAMRiseGB, hf.VRAMRiseGB = round1(h.RAMRise.Percentile(humanPct)), round1(h.VRAMRise.Percentile(humanPct))
			hf.NetMbps = round1(h.Net.Percentile(humanPct))
		}
		f.Human = append(f.Human, hf)
	}
	for key, h := range d.st.History {
		if h.RAM == nil || h.RAM.N == 0 {
			continue
		}
		project := key
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				project = key[:i]
				break
			}
		}
		name := h.Name
		if name == "" {
			name = fmt.Sprintf("command %.8s", key[len(project):])
		}
		f.Commands = append(f.Commands, CommandForecast{Project: project, Name: name, Runs: h.Runs,
			RAMGB: round1(usage.PeakNeed(h.RAM, 0, 0)), VRAMGB: round1(usage.PeakNeed(h.VRAM, 0, 0)),
			CPUs: round1(usage.RateNeed(h.CPU, 0)), GPUPct: math.Round(usage.RateNeed(h.GPU, 0)),
			DiskGB: round1(usage.PeakNeed(h.Disk, 0, 0)), NetMbps: round1(usage.RateNeed(h.Net, 0)),
			Minutes: round1(usage.RateNeed(h.Mins, 0)), Warm: h.RAM.N >= usage.WarmRuns})
	}
	sort.Slice(f.Commands, func(a, b int) bool {
		if f.Commands[a].Project != f.Commands[b].Project {
			return f.Commands[a].Project < f.Commands[b].Project
		}
		return f.Commands[a].Name < f.Commands[b].Name
	})
	return f
}
