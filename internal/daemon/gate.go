package daemon

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
)

type BlockKind uint8

const (
	blockNone BlockKind = iota
	blockUpdating
	blockPacing
	blockNetwork
	blockBackoff
	blockHuman
	blockGPUSlot
	blockGPUPacking
	blockOutsideGPU
	blockGPUReading
	blockGPUVendor
	blockVRAM
	blockBackfill
	blockStorage
	blockDisk
	blockCPU
	blockRAM
)

type Decision struct {
	Kind         BlockKind
	Text         string
	Forecastable bool
	Card         int
	retryIn      time.Duration
	backfilled   bool
}

func blocked(kind BlockKind, text string, forecastable bool) Decision {
	return Decision{Kind: kind, Text: text, Forecastable: forecastable, Card: -1}
}

func (decision Decision) startsIn(sim float64) float64 {
	if !decision.Forecastable {
		return -1
	}
	if decision.retryIn > 0 {
		return math.Max(sim, decision.retryIn.Minutes())
	}
	return sim
}

// admitCtx tracks this pass's resource reservations and the heads that later jobs must not starve.
// A read-only pass uses the same gates and current reservations without granting any jobs.
type admitCtx struct {
	now             time.Time
	updating        bool
	ram, vram       float64
	vramSize        float64
	margin          float64
	settling        Decision
	active          int
	gpuRunning      int
	outsideGPUSlots int
	fallbackPacking bool
	ramBlocked      bool
	vramHeads       map[int]Decision
	cpuRAMHead      Decision
	gpuRAMHead      Decision
}

func (d *Daemon) newAdmitCtx(now time.Time) *admitCtx {
	ctx := &admitCtx{now: now, updating: d.updating.Load()}
	if ctx.updating {
		return ctx
	}
	ctx.ram, ctx.vram = d.headroom(now)
	_, ctx.vramSize = d.snap.MaxFreeVRAM()
	ctx.margin, ctx.settling = d.admitMargin(), d.ramSettling(now)
	for _, j := range d.jobs {
		if j.State == "running" || j.State == "granted" {
			ctx.active++
			if j.GPU {
				ctx.gpuRunning++
			}
		}
	}
	if len(d.snap.GPUs) == 0 {
		busy := d.gpuBusySlots()
		ctx.outsideGPUSlots, ctx.gpuRunning = busy-ctx.gpuRunning, busy
		ctx.fallbackPacking = busy > 0 && math.Max(d.utilEWMA, d.snap.GPUUtil()*0.5) >= d.cfg.GPU.PackBelowUtil
	}
	return ctx
}

// waitingDecision walks only as far as the candidate so it sees the same backfill heads as admit.
func (d *Daemon) waitingDecision(j *job, now time.Time) Decision {
	ctx := d.newAdmitCtx(now)
	if !ctx.updating && j.State == "waiting" {
		waiting := policy.Order(d.cfg, d.queued(), d.st.Accounts, d.capacity(), now, nil)
		for _, ticket := range d.gateOrder(waiting) {
			decision := d.gate(d.jobs[ticket.ID], ctx)
			if ticket.ID == j.ID {
				return decision
			}
		}
	}
	return d.gate(j, ctx)
}

// gateOrder reuses the latest admission draw for backfill without drawing from the scheduler's RNG.
func (d *Daemon) gateOrder(waiting []*policy.Ticket) []*policy.Ticket {
	if d.cfg.Order.Policy != "lottery" || len(d.admissionDraw) == 0 {
		return waiting
	}
	rank := make(map[string]int, len(d.admissionDraw))
	for index, ticket := range d.admissionDraw {
		rank[ticket.ID] = len(d.admissionDraw) - index
	}
	ordered := append([]*policy.Ticket(nil), waiting...)
	sort.SliceStable(ordered, func(a, b int) bool { return rank[ordered[a].ID] > rank[ordered[b].ID] })
	return ordered
}

// gate returns the first hold in admission order. Only a head blocked at a memory gate reserves
// a backfill window for jobs later in this pass. Budget bands order the pass, not eligibility.
func (d *Daemon) gate(j *job, ctx *admitCtx) Decision {
	if ctx.updating {
		return blocked(blockUpdating, "waiting for firefighter to restart for an update", false)
	}
	stamp := d.unknownAt[j.GPU]
	if d.st.History[histKey(j.Ticket)] == nil && ctx.now.Sub(stamp) < 5*time.Second {
		decision := blocked(blockPacing, "first-time commands start 5 s apart so their real use shows", true)
		decision.retryIn = 5*time.Second - ctx.now.Sub(stamp)
		return decision
	}
	overdue := ctx.now.Sub(j.Created).Minutes() >= d.cfg.Order.MaxWaitMinutes
	if j.Net {
		if why, humanOnly := d.networkHold(); why != "" && !(overdue && humanOnly) {
			return blocked(blockNetwork, why, false)
		}
	}
	if decision := d.backoffDecision(j, ctx.now); decision.Kind != blockNone {
		return decision
	}

	head := &ctx.cpuRAMHead
	decision := Decision{Text: "starting", Forecastable: true, Card: -1}
	if j.GPU {
		head = &ctx.gpuRAMHead
		decision = d.gpuGate(j, ctx, *head)
		if decision.Kind != blockNone {
			return decision
		}
	} else if head.Kind != blockNone && !shortJob(j.Ticket) {
		return backfillHold("RAM", *head)
	}
	if why := d.storageWhy(j); why != "" {
		return blocked(blockStorage, why, false)
	}
	if why := d.diskWhy(j); why != "" {
		return blocked(blockDisk, why, false)
	}
	if why := d.cpuWhy(j); why != "" {
		return blocked(blockCPU, why, true)
	}
	if hold := d.ramGate(j, ctx, overdue); hold.Kind != blockNone {
		ctx.ramBlocked = true
		if overdue && head.Kind == blockNone {
			*head = hold
		}
		return hold
	}
	return decision
}

func (d *Daemon) gpuGate(j *job, ctx *admitCtx, ramHead Decision) Decision {
	if d.gpuHeld() {
		return blocked(blockHuman, "the human needs the GPU ("+d.why()+")", false)
	}
	limit := d.cfg.GPU.MaxJobs
	if len(d.snap.GPUs) == 0 {
		switch {
		case ctx.outsideGPUSlots >= limit && ctx.outsideGPUSlots > 0,
			ctx.fallbackPacking && ctx.outsideGPUSlots > 0:
			return d.outsideGPUHold()
		case ctx.gpuRunning >= limit:
			return blocked(blockGPUSlot, fmt.Sprintf("GPU turn queue: the GPU slots are full (gpu.max_jobs = %d)", limit), true)
		case ctx.fallbackPacking:
			return blocked(blockGPUPacking, "GPU packing: utilization is at or above gpu.pack_below_util", true)
		}
	}
	if d.gpuDegraded {
		return blocked(blockGPUReading, "waiting for GPU readings (only some GPUs are reporting)", false)
	}
	if ramHead.Kind != blockNone && !shortJob(j.Ticket) {
		return backfillHold("RAM", ramHead)
	}
	card, left := d.cardFor(j.Ticket)
	if card < 0 && len(d.snap.GPUs) > 0 {
		return blocked(blockGPUVendor, "waiting for a "+j.WantVendor+" GPU (none is reporting right now)", false)
	}
	if card >= 0 {
		switch d.cardBlock(card) {
		case blockOutsideGPU:
			return d.outsideGPUHold()
		case blockGPUSlot:
			return blocked(blockGPUSlot, fmt.Sprintf("GPU turn queue: the selected card's slots are full (gpu.max_jobs = %d)", limit), true)
		case blockGPUPacking:
			return blocked(blockGPUPacking, "GPU packing: card utilization is at or above gpu.pack_below_util", true)
		}
		// Measured, not feared: the card's smoothed load now plus this job's usual load. Two jobs
		// past 100% only share the card's time (each runs slower, none fails), so the line is 150%:
		// a half-busy card takes a second job (Josh 11/22: data-pow10 held at ~50% busy).
		if now := d.measuredUtil(card); d.cardJobs(card) > 0 && j.PredGPU > 0 && now+j.PredGPU > 150 {
			return blocked(blockGPUPacking, fmt.Sprintf("GPU packing: the card is %.0f%% busy (measured, last 30 s) and this job usually adds %.0f%%: more than 150%% would slow both a lot", now, j.PredGPU), true)
		}
	}
	left -= d.res.VRAMGB
	need := d.needOn(j.Ticket, card)
	if d.waitsForAwayOn(j.Ticket, card, need) {
		return blocked(blockHuman, "needs more VRAM than the card frees beside the human's reserve: starts while the human is away", false)
	}
	if ctx.vramSize > 0 && card >= 0 && need > left {
		free := math.Max(0, left)
		why := fmt.Sprintf("needs %.1f GB VRAM, %.1f GB free after the human's reserve", need, free) +
			nearMiss(j.ID, "vram", need, j.VRAMGB, free)
		hold := blocked(blockVRAM, why, d.vramHeldOn(card) >= need-left)
		hold.Card = card
		// Like RAM: a blocked job holds the card for itself (only short jobs pass it) once it has
		// waited order.max_wait_minutes. Before that, any job that fits starts beside it (Josh
		// 11/22: a 3 GB job waited behind an 8.5 GB one that couldn't start).
		overdue := ctx.now.Sub(j.Created).Minutes() >= d.cfg.Order.MaxWaitMinutes
		if _, exists := ctx.vramHeads[card]; !exists && overdue {
			if ctx.vramHeads == nil {
				ctx.vramHeads = make(map[int]Decision)
			}
			ctx.vramHeads[card] = hold
		}
		return hold
	}
	vramHead, waiting := ctx.vramHeads[card]
	if waiting && !shortJob(j.Ticket) {
		return backfillHold("VRAM on this card", vramHead)
	}
	return Decision{Text: "GPU turn queue", Forecastable: true, Card: card, backfilled: waiting || ramHead.Kind != blockNone}
}

func (d *Daemon) ramGate(j *job, ctx *admitCtx, overdue bool) Decision {
	lone := overdue && ctx.active == 0 && j.NeedRAM <= ctx.ram
	if need := j.NeedRAM + ctx.margin; need > ctx.ram && !lone {
		why := d.waitRAMWhy(j, ctx) + nearMiss(j.ID, "ram", j.NeedRAM, j.RAMGB, ctx.ram-ctx.margin)
		hold := blocked(blockRAM, why, d.ramHeld() >= need-ctx.ram)
		if ctx.settling.Kind != blockNone {
			hold.Forecastable = hold.Forecastable && ctx.settling.Forecastable
			hold.retryIn = ctx.settling.retryIn
		}
		return hold
	}
	return ctx.settling
}

func (d *Daemon) ramHeld() float64 {
	held := 0.0
	for _, j := range d.jobs {
		if j.State == "running" || j.State == "granted" {
			held += j.ramGB + d.reservedRAM(j)
		}
	}
	return held
}

func (d *Daemon) vramHeldOn(card int) float64 {
	held := 0.0
	for _, j := range d.jobs {
		if j.GPU && (j.State == "running" || j.State == "granted") && d.onCard(j, card) {
			held += math.Max(j.vramGB, j.NeedVRAM)
		}
	}
	return held
}

func backfillHold(resource string, head Decision) Decision {
	hold := blocked(blockBackfill, "backfill: only short jobs can start ahead of the job waiting for "+resource, head.Forecastable)
	hold.retryIn = head.retryIn
	return hold
}

func (d *Daemon) backoffDecision(j *job, now time.Time) Decision {
	left := d.backoffLeft(j.ID, now)
	if left <= 0 {
		return Decision{}
	}
	backoff := d.backoff[j.ID]
	why := backoff.why
	if why == "" {
		why = "for the human"
	}
	text := fmt.Sprintf("stopped %s ago (%s); tries again in %s (stopped %d time(s) in a row; the wait doubles each time, up to 30 min)",
		now.Sub(backoff.at).Round(time.Second), why, left.Round(time.Second), backoff.n)
	hold := blocked(blockBackoff, text, true)
	hold.retryIn = left
	return hold
}

func (d *Daemon) outsideGPUHold() Decision {
	why := "GPU busy outside firefighter"
	if users := d.busyOutside(); len(users) > 0 {
		why += ": " + strings.Join(users, ", ")
	}
	return blocked(blockOutsideGPU, why, false)
}
