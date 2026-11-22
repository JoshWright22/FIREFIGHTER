package daemon

// Utilization: how long each resource sat idle while a job that wanted it waited (Josh 11/22: the
// GPU idle 15+ minutes with three GPU jobs waiting, and nothing in status said so).

import (
	"fmt"
	"strings"
	"time"
)

// UtilLog is one resource's last hour, a slot per minute: seconds sampled, seconds idle while a
// job of its kind waited, and what held the first waiting job at the last such sample.
type UtilLog struct {
	Minute  int64       `json:"minute"` // unix minute of slot 0's last write
	Seen    [60]float64 `json:"seen"`
	Idle    [60]float64 `json:"idle"`
	Why     string      `json:"why,omitempty"` // the id of the first job waiting at the last idle sample
	IdleNow bool        `json:"idle_now,omitempty"`
}

func (u *UtilLog) add(now time.Time, dt float64, idle bool, why string) {
	m := now.Unix() / 60
	if m != u.Minute {
		for k := u.Minute + 1; k <= m && k <= u.Minute+60; k++ {
			u.Seen[k%60], u.Idle[k%60] = 0, 0
		}
		if m-u.Minute > 60 {
			u.Seen, u.Idle = [60]float64{}, [60]float64{}
		}
		u.Minute = m
	}
	u.Seen[m%60] += dt
	u.IdleNow = idle
	if idle {
		u.Idle[m%60] += dt
		u.Why = why
	}
}

// idleMinutes is the last hour's idle-while-waiting time, in minutes.
func (u *UtilLog) idleMinutes() float64 {
	s := 0.0
	for _, v := range u.Idle {
		s += v
	}
	return s / 60
}

// sampleUtil records, once per loop tick, which resources sit idle while work for them waits.
func (d *Daemon) sampleUtil(now time.Time, dt float64) {
	if dt <= 0 || dt > 30 {
		return
	}
	if d.st.Util == nil {
		d.st.Util = map[string]*UtilLog{}
	}
	var gpuWait, cpuWait, netWait *job
	for _, t := range d.waiting() {
		j := d.jobs[t.ID]
		switch {
		case t.GPU && gpuWait == nil:
			gpuWait = j
		case !t.GPU && cpuWait == nil:
			cpuWait = j
		}
		if t.Net && netWait == nil {
			netWait = j
		}
	}
	gpuJobs := 0
	cpuFree := float64(d.snap.CPUs) - d.snap.CPUBusy - d.res.CPUs
	for _, j := range d.jobs {
		if j.GPU && (j.State == "granted" || (j.State == "running" && j.suspended.IsZero())) {
			gpuJobs++
		}
	}
	log := func(k string, idle bool, j *job) {
		u := d.st.Util[k]
		if u == nil {
			u = &UtilLog{}
			d.st.Util[k] = u
		}
		why := ""
		if idle && j != nil {
			why = j.ID // what holds it is read at status time: walking the queue every tick costs too much
		}
		u.add(now, dt, idle, why)
	}
	log("gpu", gpuWait != nil && len(d.snap.GPUs) > 0 && (gpuJobs == 0 || d.snap.GPUUtil() < 50), gpuWait)
	log("cpu", cpuWait != nil && cpuFree >= 2, cpuWait)
	log("net", netWait != nil && (d.netCap <= 0 || d.snap.NetMbps < 0.3*d.netCap), netWait)
}

// utilLines are status's "idle with work waiting" lines (none when nothing sat idle).
func (d *Daemon) utilLines() []string {
	var out []string
	for _, k := range []string{"gpu", "cpu", "net"} {
		u := d.st.Util[k]
		if u == nil {
			continue
		}
		m := u.idleMinutes()
		if m < 0.5 && !u.IdleNow {
			continue
		}
		line := fmt.Sprintf("%s idle %.0f of the last 60 min with jobs waiting", strings.ToUpper(k), m)
		if j := d.jobs[u.Why]; u.IdleNow && j != nil && j.State == "waiting" {
			line += "; now " + j.Name + ": " + d.blocker(j)
		}
		out = append(out, line)
	}
	return out
}
