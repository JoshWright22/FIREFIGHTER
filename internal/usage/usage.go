// Package usage is what firefighter learns about how much of each resource the human and each
// command use, and the predictors that turn that history into a reserve or an admission need. No
// I/O. The algorithms are the ones production autoscalers use, scaled down to one PC; each function
// names its source, and docs/research.md has the papers.
package usage

import (
	"math"
	"sort"
	"time"
)

// Hist is a decaying histogram with exponential buckets, as in Google's Autopilot (Rzadca et al.,
// EuroSys 2020, section 3.2) and the Kubernetes Vertical Pod Autoscaler built after it: each bucket
// is 5% wider than the one before (VPA's DefaultHistogramBucketSizeGrowth), and a sample of age t
// counts with weight 2^(-t/halfLife) (Autopilot Eq. 1). Decay is applied by scaling every bucket
// (Age for time, Scale for "per run"), so old behaviour fades instead of being cut off.
type Hist struct {
	First   float64         `json:"first"`             // upper edge of bucket 0: the smallest value told apart
	Buckets map[int]float64 `json:"buckets,omitempty"` // bucket -> decayed weight
	Weight  float64         `json:"weight"`            // sum of the bucket weights
	N       float64         `json:"n"`                 // samples ever added (not decayed): how much history there is
	At      time.Time       `json:"at,omitempty"`      // when it was last aged
	Since   time.Time       `json:"since,omitempty"`   // first sample
}

const growth = 1.05

// NewHist makes a histogram whose first bucket ends at first (values below it are "about zero").
func NewHist(first float64) *Hist { return &Hist{First: first, Buckets: map[int]float64{}} }

func (h *Hist) bucket(v float64) int {
	if v <= h.First {
		return 0
	}
	return int(math.Ceil(math.Log(v/h.First) / math.Log(growth)))
}

// edge is the upper edge of bucket k: what a percentile in that bucket is reported as (never under
// the samples it holds).
func (h *Hist) edge(k int) float64 { return h.First * math.Pow(growth, float64(k)) }

// Add records a sample with a weight.
func (h *Hist) Add(v, weight float64, now time.Time) {
	if h.Buckets == nil {
		h.Buckets = map[int]float64{}
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || weight <= 0 {
		return
	}
	if h.Since.IsZero() {
		h.Since, h.At = now, now
	}
	h.Buckets[h.bucket(math.Max(0, v))] += weight
	h.Weight += weight
	h.N++
}

// Scale multiplies every weight by f (0 < f < 1): one step of decay. Buckets that fade to nothing go.
func (h *Hist) Scale(f float64) {
	if f >= 1 || f <= 0 {
		return
	}
	h.Weight = 0
	for k, w := range h.Buckets {
		if w *= f; w < 1e-6 {
			delete(h.Buckets, k)
			continue
		}
		h.Buckets[k] = w
		h.Weight += w
	}
}

// Age decays the histogram for the time passed since it was last aged (Autopilot Eq. 1:
// w = 2^(-age/halfLife)).
func (h *Hist) Age(now time.Time, halfLife time.Duration) {
	if h.At.IsZero() || halfLife <= 0 || !now.After(h.At) {
		return
	}
	if dt := now.Sub(h.At); dt >= halfLife/200 { // not on every sample: the scale touches every bucket
		h.Scale(math.Pow(2, -float64(dt)/float64(halfLife)))
		h.At = now
	}
}

func (h *Hist) keys() []int {
	ks := make([]int, 0, len(h.Buckets))
	for k := range h.Buckets {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

// Percentile is the value at or under which share p (0-1) of the decayed samples fall.
func (h *Hist) Percentile(p float64) float64 { return h.percentile(p, false) }

// LoadPercentile is Autopilot's load-adjusted percentile (Eq. 3): each bucket is weighted by its
// value as well as its count, so the answer covers share p of the work done, not of the time. A
// program idle for nine minutes and flat out for one has a time-based P90 near zero and a
// load-adjusted P90 at its full load: the right number to keep free for bursty, interactive use.
func (h *Hist) LoadPercentile(p float64) float64 { return h.percentile(p, true) }

func (h *Hist) percentile(p float64, byLoad bool) float64 {
	if h == nil || h.Weight <= 0 {
		return 0
	}
	ks := h.keys()
	total := 0.0
	for _, k := range ks {
		w := h.Buckets[k]
		if byLoad {
			w *= h.edge(k)
		}
		total += w
	}
	acc, want := 0.0, p*total
	for _, k := range ks {
		w := h.Buckets[k]
		if byLoad {
			w *= h.edge(k)
		}
		if acc += w; acc >= want-1e-12 {
			if k == 0 {
				return 0 // under the first edge: about nothing
			}
			return h.edge(k)
		}
	}
	return h.edge(ks[len(ks)-1])
}

// Max is the highest value still remembered (Autopilot's S_max: the highest non-empty bucket).
func (h *Hist) Max() float64 {
	if h == nil || len(h.Buckets) == 0 {
		return 0
	}
	ks := h.keys()
	if k := ks[len(ks)-1]; k > 0 {
		return h.edge(k)
	}
	return 0
}

// Min is the lowest value still remembered (the lower edge of the lowest non-empty bucket).
func (h *Hist) Min() float64 {
	if h == nil || len(h.Buckets) == 0 {
		return 0
	}
	if k := h.keys()[0]; k > 0 {
		return h.edge(k - 1)
	}
	return 0
}

// Mean is the decayed average (Autopilot's S_avg, Eq. 2).
func (h *Hist) Mean() float64 {
	if h == nil || h.Weight <= 0 {
		return 0
	}
	sum := 0.0
	for k, w := range h.Buckets {
		if k > 0 {
			sum += w * h.edge(k)
		}
	}
	return sum / h.Weight
}

// Days of history behind the histogram: VPA's confidence (getConfidence: the lesser of the time
// span covered and the samples received, in days, for samples arriving every `every`).
func (h *Hist) Days(now time.Time, every time.Duration) float64 {
	if h == nil || h.Since.IsZero() || every <= 0 {
		return 0
	}
	span := now.Sub(h.Since).Hours() / 24
	got := h.N * every.Hours() / 24
	return math.Max(0, math.Min(span, got))
}

// ---- predictors -------------------------------------------------------------------------------

// Margins and percentiles, with their sources.
const (
	// SafetyMargin is added to every learned value: Autopilot adds 10-15% (section 3.2.2), VPA's
	// recommendation-margin-fraction is 0.15.
	SafetyMargin = 0.15
	// YoungMargin is the margin while a command has run only once or twice: the 1.2 firefighter
	// has always used, which is also VPA's oom-bump-up-ratio.
	YoungMargin = 0.20
	// WarmRuns is how many runs make a command's history trusted over its declaration. "Take it to
	// the limit" (Bashir et al., EuroSys 2021, section 4) counts a task at its declared limit until
	// it has min_num_samples of history; Tsafrir et al. predict from the last 2 runs.
	WarmRuns = 3
	// PeakPercentile for memory and VRAM: Autopilot uses P98 of the peaks for jobs that tolerate a
	// rare out-of-memory, the maximum for those that don't (section 3.2.2).
	PeakPercentile = 0.98
	// RunHalfLifePeak and RunHalfLifeRate: how many runs until a sample counts half. Autopilot
	// forgets memory four times slower than CPU (48 h against 12 h); in runs, 8 against 2.
	RunHalfLifePeak = 8.0
	RunHalfLifeRate = 2.0
	// OOMBump: after a job ran out of memory or was stopped for it, its next need is at least
	// max(peak x 1.2, peak + 100 MiB) (VPA RecordOOM: oom-bump-up-ratio, oom-min-bump-up-bytes).
	OOMBumpRatio = 1.2
	OOMBumpGB    = 100.0 / 1024
)

// PeakNeed is the admission need for a resource a job must not run out of (RAM, VRAM, disk space),
// from the histogram of its past runs' peaks.
//
//   - No history: the declaration, or the cold-start value (every production system surveyed falls
//     back to the declared limit with no history).
//   - One or two runs: the larger of the declaration and the highest peak seen x 1.2 (Autopilot's
//     S_max, for work that can't tolerate running out).
//   - WarmRuns or more: P98 of the peaks x 1.15 (Autopilot S_p98 + margin). A declaration above
//     anything ever seen is cut to 1.5 x the highest peak, so a habitual over-ask doesn't hold
//     memory nobody uses; one below it is ignored.
func PeakNeed(h *Hist, declared, coldStart float64) float64 {
	if h == nil || h.N == 0 {
		if declared > 0 {
			return declared
		}
		return coldStart
	}
	top := h.Max()
	if h.N < WarmRuns {
		return math.Max(declared, top*(1+YoungMargin))
	}
	need := h.Percentile(PeakPercentile) * (1 + SafetyMargin)
	if top <= 0 {
		return math.Max(need, declared) // never measured (a container, a card without counters): trust the declaration
	}
	if declared > need {
		need = math.Max(need, math.Min(declared, top*1.5))
	}
	return need
}

// RateNeed is what a job is expected to use of a resource it can be throttled on (CPUs, network,
// disk writes): its decayed mean (Autopilot uses S_avg for batch CPU: "if a job tolerates CPU
// throttling, the most efficient limit is the job's average load"). cold is used with no history.
func RateNeed(h *Hist, cold float64) float64 {
	if h == nil || h.N == 0 {
		return cold
	}
	return h.Mean()
}

// AddRun records one finished run's value in a per-run histogram: older runs fade by
// 2^(-1/halfLifeRuns) first.
func AddRun(h *Hist, v, halfLifeRuns float64, now time.Time) {
	h.Scale(math.Pow(2, -1/halfLifeRuns))
	h.Add(v, 1, now)
}

// OOMBump is the artificial peak recorded for a run that was stopped for lack of memory: its real
// need is above what it reached (VPA RecordOOM).
func OOMBump(peak float64) float64 { return math.Max(peak*OOMBumpRatio, peak+OOMBumpGB) }

// Reserve sizes one part of the human's reserve from what the human has been seen to use in the
// current state. prior is the configured value: it is used alone until there is a day of history
// (VPA's confidence: with little history the estimate is not trusted), and afterwards it bounds
// the learned value to between half and three times itself, so a quiet week can't strip the human
// of headroom and one heavy day can't lock the machine up. learned is already the percentile the
// caller wants; margin is added to it.
func Reserve(prior, learned, days, margin float64) float64 {
	if days < 1 || prior <= 0 {
		return prior
	}
	return math.Max(prior/2, math.Min(prior*3, learned*(1+margin)))
}

// NSigma is the peak predictor of "Take it to the limit" (EuroSys 2021, section 4): mean + n
// standard deviations of a total, which under the central limit theorem is about P99 for n = 3.
func NSigma(samples []float64, n float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	mean := 0.0
	for _, s := range samples {
		mean += s
	}
	mean /= float64(len(samples))
	v := 0.0
	for _, s := range samples {
		v += (s - mean) * (s - mean)
	}
	return mean + n*math.Sqrt(v/float64(len(samples)))
}

// Growth tracks how much a quantity (the human's RAM, their VRAM) rises within a horizon: the
// headroom that has to be free now so the rise never finds the machine full. Each reading that
// leaves the window yields one sample: the highest value seen since it, minus it.
type Growth struct {
	vals []growthAt
}

type growthAt struct {
	at    time.Time
	v     float64
	label string
}

// Push adds a reading and returns the growth samples that became complete: for each reading now
// older than horizon, how far the quantity rose above it within the horizon, with that reading's
// label (the user state then).
func (g *Growth) Push(now time.Time, v float64, label string, horizon time.Duration) (rises []float64, labels []string) {
	g.vals = append(g.vals, growthAt{now, v, label})
	for len(g.vals) > 1 && now.Sub(g.vals[0].at) >= horizon {
		old := g.vals[0]
		g.vals = g.vals[1:]
		top := old.v
		for _, x := range g.vals {
			if x.at.Sub(old.at) <= horizon {
				top = math.Max(top, x.v)
			}
		}
		rises, labels = append(rises, top-old.v), append(labels, old.label)
	}
	return
}
