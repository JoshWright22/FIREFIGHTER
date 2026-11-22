package daemon

// The human's own CPU use, for the CPU reserve (Josh 11/22, urgent: "reserve for the human: 13
// CPUs" while the machine was 38% busy and the human only browsing; GPU jobs waited for CPUs).
// The reserve used to follow everything that wasn't a measured job: agent CLIs and their tools,
// and a job's children in the seconds before they were attributed to it, all counted as the
// human's, and one spike lifted the reserve to reserve.max_cpu_pct for minutes. Now it follows a
// 30-second average of the CPU used by processes that are neither firefighter jobs nor anything
// under an agent CLI: the human's browser, chat, video, games and builds.

import (
	"math"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// humanCPUState is the measurement, kept between loop passes.
type humanCPUState struct {
	scanner *sysinfo.Scanner
	at      time.Time
	agent   map[int]agentMark // pid -> is it under an agent CLI (pid reuse: checked by start time)
	cleared time.Time
	ewma    float64
	ok      bool // at least one scan done
}

type agentMark struct {
	created int64
	agent   bool
}

// measureHumanCPU scans every process at most every 5 s, outside the daemon lock.
func (d *Daemon) measureHumanCPU(now time.Time, cfg config.Config, governed map[int]bool) {
	h := &d.humanCPU
	if now.Sub(h.at) < 5*time.Second {
		return
	}
	dt := now.Sub(h.at).Seconds()
	h.at = now
	if h.scanner == nil {
		h.scanner = sysinfo.NewScanner()
	}
	if h.agent == nil || now.Sub(h.cleared) > 10*time.Minute {
		h.agent, h.cleared = map[int]agentMark{}, now
	}
	procs := h.scanner.Scan(0.02, math.Inf(1))
	sum := 0.0
	for _, p := range procs {
		if governed[p.PID] {
			continue // a job, or firefighter itself
		}
		m, seen := h.agent[p.PID]
		if !seen || m.created != sysinfo.Created(p.PID) {
			m = agentMark{created: sysinfo.Created(p.PID), agent: sysinfo.AgentAncestorPID(p.PID, cfg.Guard.AgentParents) != 0}
			h.agent[p.PID] = m
		}
		if !m.agent {
			sum += p.CPU
		}
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return
	}
	if !h.ok || dt > 60 {
		h.ewma, h.ok = sum, true // the first scan has no interval yet: it reads as 0, which is fine
		return
	}
	h.ewma += (1 - math.Exp(-dt/30)) * (sum - h.ewma)
}
