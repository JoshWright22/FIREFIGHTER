package daemon

// Network admission and human bandwidth protection.

import (
	"fmt"
	"time"
)

// netMeasure is how long the human's own traffic is measured, with no network job running, before
// the next download may start (after the last one ends, and after the daemon starts).
const netMeasure = 30 * time.Second

// networkHold returns the reason and whether only the human's measured share holds the job back.
// Overdue jobs may bypass that estimate: the link's size is learned and may be too small at first.
func (d *Daemon) networkHold() (why string, humanOnly bool) {
	c := d.cfg.Network
	n := 0
	for _, j := range d.jobs {
		if j.Net && (j.State == "running" || j.State == "granted") {
			n++
		}
	}
	switch {
	case c.MaxJobs > 0 && n >= c.MaxJobs:
		return "network turn: another download has it", false
	case d.user == "game" && c.PauseInGame:
		return "network: the human is gaming (latency)", false
	case n == 0 && time.Since(d.netQuietFrom) < netMeasure:
		return "network: measuring the human's own traffic first (30 s)", false
	case d.netCap > 1 && d.netHuman > d.netCap*c.HumanBusyPct/100:
		return fmt.Sprintf("network: the human is using %.0f of ~%.0f Mbps", d.netHuman, d.netCap), true
	}
	return "", false
}
