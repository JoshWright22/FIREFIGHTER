package daemon

// GPU placement, card capacity, and VRAM accounting.

import (
	"math"
	"strings"

	"github.com/JoshWright22/firefighter/internal/policy"
)

func anyGPUJobRunning(d *Daemon) bool {
	for _, j := range d.running() {
		if j.GPU {
			return true
		}
	}
	return false
}

// vramShortCard returns the GPU whose free VRAM is under the human's reserve while firefighter
// jobs run on it, or -1.
func (d *Daemon) vramShortCard() int {
	waitingGPU := false
	for _, t := range d.waiting() {
		if t.GPU {
			waitingGPU = true
		}
	}
	// Away with nobody waiting for VRAM: a job that fills the card by design (TensorFlow's default)
	// hurts no one, so don't evict it in a loop.
	if d.nobodyAtKeyboard() && !waitingGPU {
		return -1
	}
	for i, g := range d.snap.GPUs {
		if g.VRAMGB-g.UsedGB >= d.res.VRAMGB {
			continue
		}
		for _, j := range d.running() {
			if j.GPU && d.onCard(j, i) {
				return i
			}
		}
	}
	return -1
}

// onCard says whether a job runs (or may run) on GPU i. Jobs placed nowhere in particular use the
// default device, 0.
func (d *Daemon) onCard(j *job, i int) bool {
	if j.device >= 0 {
		return j.device == i
	}
	return i == 0 || len(d.snap.GPUs) <= 1
}

// gpuBusySlots counts GPU slots taken: firefighter GPU jobs (running or granted) plus heavy GPU
// compute outside firefighter.
func (d *Daemon) gpuBusySlots() int {
	n := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") {
			n++
		}
	}
	for _, u := range d.foreign {
		if u >= d.cfg.Reserve.ForeignGPUPct {
			n++
		}
	}
	return n
}

// freestCard is the GPU with the most free VRAM after its jobs' unclaimed needs, and its raw free
// VRAM (-1, 0 with no GPU).
func (d *Daemon) freestCard() (int, float64) {
	best, bestLeft, bestFree := -1, 0.0, 0.0
	for i, g := range d.snap.GPUs {
		free := g.VRAMGB - g.UsedGB
		left := free
		for _, j := range d.jobs {
			if j.GPU && (j.State == "granted" || j.State == "running") && d.onCard(j, i) {
				left -= d.reservedVRAM(j)
			}
		}
		if best < 0 || left > bestLeft {
			best, bestLeft, bestFree = i, left, free
		}
	}
	return best, bestFree
}

// cardFor picks the GPU a job should use: among the vendor it wants (see policy.Ticket.WantVendor),
// the one with the most VRAM left after jobs' unclaimed needs. Returns -1 with no GPU.
// vendorFor resolves which GPUs a job can use: its --gpu-vendor, else NVIDIA when there is one
// (most GPU code is CUDA), else any ("").
func (d *Daemon) vendorFor(want string) string {
	want = strings.ToLower(want)
	if want == "any" {
		return ""
	}
	if want == "" {
		for _, g := range d.snap.GPUs {
			if g.Vendor == "nvidia" {
				return "nvidia"
			}
		}
	}
	return want
}

// capacityOf is the most VRAM card i ever offers: its size, less the human's reserve, less the
// least others have held lately.
func (d *Daemon) capacityOf(i int) float64 {
	g := d.snap.GPUs[i]
	floor := 0.0
	if i < len(d.cardFloor) {
		floor = d.cardFloor[i]
	}
	return g.VRAMGB - d.res.VRAMGB - floor
}

// capFor is how much of card i a ticket can count on: for a need its own --vram covers, the card's
// size less the human's reserve (it waits for others to leave); for a learned need, what the card
// has actually freed lately (capacityOf).
func (d *Daemon) capFor(i int, t *policy.Ticket) float64 {
	if t.VRAMGB > 0 && t.NeedVRAM <= t.VRAMGB {
		return d.snap.GPUs[i].VRAMGB - d.res.VRAMGB
	}
	return d.capacityOf(i)
}

// cardFor picks the GPU for a ticket: among the cards of the vendor it can use, the freest one that
// can hold it (capFor); if none can, the one that offers the most (needOn then clamps the need to
// it). Returns the card and its VRAM left after jobs' unclaimed needs; -1 with no card.
func (d *Daemon) cardFor(t *policy.Ticket) (int, float64) {
	want, need := t.WantVendor, t.NeedVRAM
	v := d.vendorFor(want)
	best, bestLeft, bestFits, bestOpen, bestRoom := -1, 0.0, false, false, false
	for i, g := range d.snap.GPUs {
		if v != "" && g.Vendor != v {
			continue
		}
		left := g.VRAMGB - g.UsedGB
		for _, j := range d.jobs {
			if j.GPU && (j.State == "granted" || j.State == "running") && d.onCard(j, i) {
				left -= d.reservedVRAM(j)
			}
		}
		fits, open := d.capFor(i, t) >= need, d.cardOpen(i)
		// Room now: VRAM free beyond the human's reserve for this need. An empty card that is short
		// of VRAM must not win over a card with one light job and room (review 11/20: the job then
		// waited on the empty card while the other could take it).
		room := left-d.res.VRAMGB >= need
		switch {
		case best < 0, fits && !bestFits: // a card that can hold it first: it waits for that one
		case fits != bestFits:
			continue
		case open && !bestOpen: // then one that takes a job now
		case open != bestOpen:
			continue
		case room && !bestRoom: // then one with the VRAM for it now
		case room != bestRoom:
			continue
		case fits && d.lighter(i, best, left, bestLeft): // among cards that fit: the least loaded
		case !fits && d.capFor(i, t) > d.capFor(best, t): // none fits: the biggest
		default:
			continue
		}
		best, bestLeft, bestFits, bestOpen, bestRoom = i, left, fits, open, room
	}
	return best, bestLeft
}

// lighter says whether card a is a better place for a new job than card b: fewer firefighter jobs,
// then less smoothed load, then more VRAM left.
func (d *Daemon) lighter(a, b int, leftA, leftB float64) bool {
	if ja, jb := d.cardJobs(a), d.cardJobs(b); ja != jb {
		return ja < jb
	}
	if ua, ub := d.utilOf(a), d.utilOf(b); math.Abs(ua-ub) >= 10 {
		return ua < ub
	}
	return leftA > leftB
}

// cardJobs counts the GPU jobs running or granted on card i.
func (d *Daemon) cardJobs(i int) int {
	n := 0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") && d.onCard(j, i) {
			n++
		}
	}
	return n
}

// utilOf is card i's smoothed utilization (its momentary reading counts at half: a dip isn't room).
func (d *Daemon) utilOf(i int) float64 {
	u := 0.0
	if i < len(d.cardUtil) {
		u = d.cardUtil[i]
	}
	if i < len(d.snap.GPUs) {
		u = math.Max(u, d.snap.GPUs[i].Util*0.5)
	}
	return u
}

// measuredUtil is card i's utilization smoothed over the last 30 s (its reading now before the
// first smoothing).
func (d *Daemon) measuredUtil(i int) float64 {
	if i < len(d.cardUtil) {
		return d.cardUtil[i]
	}
	if i < len(d.snap.GPUs) {
		return d.snap.GPUs[i].Util
	}
	return 0
}

// cardOpen says whether card i takes another job now: fewer than gpu.max_jobs on it (heavy outside
// load on an otherwise empty card counts as one), and a busy card packs a second job only while
// its smoothed utilization leaves room.
func (d *Daemon) cardOpen(i int) bool {
	return d.cardBlock(i) == blockNone
}

func (d *Daemon) cardBlock(i int) BlockKind {
	c := d.cfg
	n := d.cardJobs(i)
	util := d.utilOf(i)
	outside := n == 0 && util >= c.Reserve.ForeignGPUPct
	if outside {
		n = 1
	}
	kind := blockNone
	if n >= c.GPU.MaxJobs {
		kind = blockGPUSlot
	} else if n > 0 && util >= c.GPU.PackBelowUtil {
		kind = blockGPUPacking
	}
	if outside && kind != blockNone {
		return blockOutsideGPU
	}
	return kind
}

// cardSize is the VRAM of the card a job runs on (fallback: the machine's), for billing.
func (d *Daemon) cardSize(j *job, fallback float64) float64 {
	for i, g := range d.snap.GPUs {
		if d.onCard(j, i) {
			return g.VRAMGB
		}
	}
	return fallback
}

// needOn is a job's VRAM need on a card. A learned need (peak x margin) can't exceed what the card
// ever offers: its size, less the human's reserve, less the least others have held lately. Above
// that it would wait forever, so it's clamped and starts when the card is at its emptiest (if a
// program holds VRAM for good, the job runs with what's there and its log says so). A declared
// --vram is measured against the card's size less the human's reserve; past that it's clamped the
// same way and waits for the human to be away.
func (d *Daemon) needOn(t *policy.Ticket, card int) float64 {
	need := t.NeedVRAM
	if card >= 0 && card < len(d.snap.GPUs) && need > d.capFor(card, t) {
		// More than this card can ever give it (no card can: cardFor prefers one that can). It
		// starts when the card is at its emptiest instead of waiting forever; queue time already
		// refused a --vram larger than every card.
		need = math.Max(0.5, d.capacityOf(card))
	}
	return need
}

// cardLeft is the VRAM a job of this vendor would find on its card, after the human's reserve.
func (d *Daemon) cardLeft(t *policy.Ticket) float64 {
	_, left := d.cardFor(t)
	return left - d.res.VRAMGB
}
