package daemon

// Network admission and human bandwidth protection.

import (
	"fmt"
	"strings"
	"time"
)

// netMeasure is how long the human's own traffic is measured, with no network job running, before
// the next download may start (after the last one ends, and after the daemon starts).
const netMeasure = 30 * time.Second

// netHumanOnly: the only thing holding network jobs back is the human's measured share of the link
// (its size is learned from peaks, so it may be too small until a download has run once).
func (d *Daemon) netHumanOnly() bool {
	return strings.HasPrefix(d.netWhy(), "network: the human is using")
}

// netFree says whether a network job may start now: a network turn is free, no game holds the
// machine, and the human isn't already using much of the link.
func (d *Daemon) netFree() bool { return d.netWhy() == "" }

// netWhy says what holds a network job back ("" when nothing does). Between two downloads it waits
// netMeasure (30 s), so the human's own traffic is measured again (it can't be told apart while one runs).
func (d *Daemon) netWhy() string {
	c := d.cfg.Network
	n := 0
	for _, j := range d.jobs {
		if j.Net && (j.State == "running" || j.State == "granted") {
			n++
		}
	}
	switch {
	case c.MaxJobs > 0 && n >= c.MaxJobs:
		return "network turn: another download has it"
	case d.user == "game" && c.PauseInGame:
		return "network: the human is gaming (latency)"
	case n == 0 && time.Since(d.netQuietFrom) < netMeasure:
		return "network: measuring the human's own traffic first (30 s)"
	case d.netCap > 1 && d.netHuman > d.netCap*c.HumanBusyPct/100:
		return fmt.Sprintf("network: the human is using %.0f of ~%.0f Mbps", d.netHuman, d.netCap)
	}
	return ""
}
