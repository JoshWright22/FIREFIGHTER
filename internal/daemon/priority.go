package daemon

// Priority holds (Josh 11/22, urgent): a bid-7 job needing 4.5 GB of RAM waited from 4:17 while a
// stream of small bid-3 to bid-5 jobs kept taking the GPU slot and the RAM as each freed. Backfill
// alone let any job that fit pass a waiting one, so under steady small-job arrival the higher bid
// never started.
//
// Now a waiting job blocked on RAM, VRAM or a GPU slot holds what it needs against every job with
// a strictly lower bid behind it: its RAM need is kept out of what they may take, and if it is a
// GPU job, so are the GPU slots. A lower-bid job still starts ahead of it when it ends first (its
// declared minutes within the held job's predicted start: EASY backfill), so nothing idles
// needlessly. Equal bids keep plain backfill; a held job that could never fit even with every
// running job gone holds nothing (it would freeze the machine for no one).

import (
	"fmt"
	"math"
	"sort"

	"github.com/JoshWright22/firefighter/internal/policy"
)

// prioHold is one higher-bid waiting job's claim during an admission pass.
type prioHold struct {
	name    string
	prio    int
	gpu     bool    // a GPU job: lower-bid GPU jobs may not take a slot
	ramGB   float64 // its RAM need, kept out of what lower-bid jobs may take
	startIn float64 // minutes until it is expected to start (what passing it must fit in)
}

// gate is admission's check for one job: priority holds of higher-bid jobs first, then its own
// resources (gateResources). A job blocked on a resource leaves a hold for the jobs behind it.
func (d *Daemon) gate(j *job, ctx *admitCtx) Decision {
	if dec := d.priorityHold(j, ctx); dec.Kind != blockNone {
		return dec
	}
	dec := d.gateResources(j, ctx)
	d.notePriorityHold(j, ctx, dec)
	return dec
}

// priorityHold says whether a higher-bid waiting job's hold keeps j back.
func (d *Daemon) priorityHold(j *job, ctx *admitCtx) Decision {
	prio := policy.Priority(j.Ticket)
	reserved, startIn, gpuHeld := 0.0, math.Inf(1), false
	var by *prioHold
	for i := range ctx.holds {
		h := &ctx.holds[i]
		if h.prio <= prio {
			continue
		}
		reserved += h.ramGB
		if h.gpu && j.GPU {
			gpuHeld = true
		}
		if h.startIn < startIn {
			startIn, by = h.startIn, h
		}
	}
	if by == nil {
		return Decision{}
	}
	ramShort := j.NeedRAM+ctx.margin > ctx.ram-reserved
	if !ramShort && !gpuHeld {
		return Decision{} // takes nothing a higher bid needs
	}
	if j.Minutes > 0 && j.Minutes <= startIn {
		return Decision{} // ends before the held job is expected to start
	}
	what := "RAM"
	if gpuHeld {
		what = "GPU slot"
	}
	hold := blocked(blockPriority, fmt.Sprintf("a bid-%d job (%s) waits for this %s and starts first; this bid-%d job passes it only if it ends before then (it declares %.0f min; that one should start in ~%.0f min)",
		by.prio, by.name, what, prio, j.Minutes, startIn), true)
	return hold
}

// notePriorityHold records a hold for a job a resource keeps waiting.
func (d *Daemon) notePriorityHold(j *job, ctx *admitCtx, dec Decision) {
	switch dec.Kind {
	case blockRAM, blockVRAM, blockGPUSlot, blockGPUPacking:
	default:
		return // not waiting on something other jobs could take (the human, the network, a backoff)
	}
	startIn, ok := d.expectedStart(j, ctx, dec.Kind)
	if !ok {
		return
	}
	ctx.holds = append(ctx.holds, prioHold{name: j.Name, prio: policy.Priority(j.Ticket), gpu: j.GPU, ramGB: j.NeedRAM, startIn: startIn})
}

// expectedStart is when j should be able to start, in minutes, from the running jobs' predicted
// ends: once enough of their RAM is back, and for a GPU job a GPU job has ended too. ok is false
// when even every running job's end wouldn't make room.
func (d *Daemon) expectedStart(j *job, ctx *admitCtx, kind BlockKind) (float64, bool) {
	type end struct{ in, ram float64 }
	var ends []end
	gpuEnd := math.Inf(1)
	for _, r := range d.jobs {
		if r.State != "running" && r.State != "granted" {
			continue
		}
		left := math.Max(1, r.PredMinutes+r.extended)
		if r.State == "running" {
			left = policy.Remaining(r.PredMinutes+r.extended, r.leaseUsed(ctx.now).Minutes())
		}
		ends = append(ends, end{left, r.ramGB + d.reservedRAM(r)})
		if r.GPU {
			gpuEnd = math.Min(gpuEnd, left)
		}
	}
	sort.Slice(ends, func(a, b int) bool { return ends[a].in < ends[b].in })
	at := 0.0
	if short := j.NeedRAM + ctx.margin - ctx.ram; short > 0 {
		freed, found := 0.0, false
		for _, e := range ends {
			if freed += e.ram; freed >= short {
				at, found = e.in, true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	if j.GPU && kind != blockRAM {
		if math.IsInf(gpuEnd, 1) {
			return 0, false // nothing of ours holds the card: what keeps it waiting isn't a job
		}
		at = math.Max(at, gpuEnd)
	}
	return at, true
}
