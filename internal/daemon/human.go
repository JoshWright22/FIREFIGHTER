package daemon

// Human activity sensing and CPU reserve enforcement.

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// waitsForAway: a GPU job that needs more than its card frees beside the human's reserve would
// only be stopped again; it gets the card at its emptiest while the human is away.
func (d *Daemon) waitsForAway(t *policy.Ticket) bool {
	card, _ := d.cardFor(t)
	return d.waitsForAwayOn(t, card, d.needOn(t, card))
}

func (d *Daemon) waitsForAwayOn(t *policy.Ticket, card int, need float64) bool {
	if d.nobodyAtKeyboard() || time.Since(t.Created).Minutes() >= d.cfg.Order.MaxWaitMinutes {
		return false // no one to wait for, or it waited its 45 minutes
	}
	return card >= 0 && need < t.NeedVRAM-0.05
}

// nobodyAtKeyboard: the human is away, or input can't be read at all (a headless box, a desktop
// without an idle source) and no game or fullscreen app is up: there's no one to make room for.
func (d *Daemon) nobodyAtKeyboard() bool {
	return d.user == "away" || (d.snap.Idle < 0 && d.user != "game")
}

// ---- the human ---------------------------------------------------------------------------------

func (d *Daemon) senseUser(now time.Time) {
	c := d.cfg
	if now.Sub(d.namesAt) > 5*time.Second {
		d.names, d.namesAt = sysinfo.Names(), now
	}
	why := ""
	if d.snap.Foreground != "" {
		why = "fullscreen " + d.snap.Foreground
	}
	for _, a := range c.Reserve.ExclusiveApps {
		if d.names[strings.ToLower(a)] || d.names[strings.ToLower(sysinfo.ExeName(a))] {
			why = a
		}
	}
	// Heavy 3D-engine use by the app in front is play (a game in a window); 3D use by background
	// processes, and CUDA compute anywhere outside firefighter, is load, not the human.
	for n, u := range d.graphics {
		if u >= c.Reserve.ForeignGPUPct && (d.snap.ForegroundApp == "" || strings.EqualFold(n, d.snap.ForegroundApp)) && d.snap.ProcUtilKnown {
			why = fmt.Sprintf("%s using %.0f%% of the 3D engine", n, u)
		}
	}
	// A fullscreen game that drops out of fullscreen for a moment (a menu, a loading screen, a
	// borderless switch) is still the game while it stays the app in front (11/20: TF2 flipped
	// game -> active -> game every 1-3 minutes).
	if d.snap.Foreground != "" {
		d.gameApp = d.snap.ForegroundApp
		if d.gameApp == "" {
			d.gameApp = d.snap.Foreground
		}
	}
	if why == "" && d.gameApp != "" && now.Before(d.gameTill) && strings.EqualFold(d.snap.ForegroundApp, d.gameApp) {
		why = d.gameWhy
	}
	if why != "" {
		d.gameWhy, d.gameTill = cleanText(why, 200), now.Add(time.Duration(c.Reserve.GameCoolSeconds*float64(time.Second)))
	} else if !now.Before(d.gameTill) {
		d.gameApp = ""
	}
	game := now.Before(d.gameTill)
	idle := d.snap.Idle
	if idle < 0 {
		idle = (c.Reserve.ActiveSeconds + c.Reserve.AwaySeconds) / 2 // unknown (headless): present
	}
	prev := d.user
	d.user = d.steadyUser(policy.UserState(c, idle, game), now)
	if d.user != prev {
		logf("user: %s -> %s %s", prev, d.user, d.gameWhy)
		d.broadcast()
	}
}

// userRank orders the human's states by how much they protect the human.
var userRank = map[string]int{"away": 0, "present": 1, "active": 2, "game": 3}

// steadyUser is the human's state with hysteresis: a step up (toward game) takes effect at once,
// since it protects the human; a step down needs the lower state in 2 of the last 3 readings,
// taken 10 s apart, so one odd reading doesn't move jobs back onto the machine.
func (d *Daemon) steadyUser(raw string, now time.Time) string {
	cur := d.user
	if cur == "" || userRank[raw] >= userRank[cur] {
		d.userSamples, d.userSampleAt = []string{raw}, now
		return raw
	}
	firstLower := true
	for _, x := range d.userSamples {
		if userRank[x] < userRank[cur] {
			firstLower = false
		}
	}
	if firstLower || now.Sub(d.userSampleAt) >= 10*time.Second {
		d.userSamples = append(d.userSamples, raw)
		if len(d.userSamples) > 3 {
			d.userSamples = d.userSamples[len(d.userSamples)-3:]
		}
		d.userSampleAt = now
	}
	lower := 0
	best := cur
	for _, s := range d.userSamples {
		if userRank[s] < userRank[cur] {
			lower++
			if userRank[s] > userRank[best] || best == cur {
				best = s // the highest of the lower readings: step down no further than they agree
			}
		}
	}
	if lower >= 2 {
		d.userSamples, d.userSampleAt = []string{best}, now
		return best
	}
	return cur
}

// gpuHeld: the human's current state reserves the whole GPU (pause_gpu).
func (d *Daemon) gpuHeld() bool { return d.cfg.ReserveFor(d.user).PauseGPU }

// enforceCPU keeps jobs on the highest-numbered CPUs, leaving the reserve idle for the human
// (PerfIso: a resized idle-core buffer protects latency where priority and rate caps don't).
func (d *Daemon) enforceCPU() {
	n := max(1, d.snap.CPUs)
	keep := int(math.Round(d.res.CPUs))
	allow := max(1, n-keep)
	cpus := make([]int, 0, allow)
	for i := n - allow; i < n; i++ {
		cpus = append(cpus, i)
	}
	key := fmt.Sprint(allow)
	for _, j := range d.running() {
		for p, created := range j.pids {
			if j.applied[p] == key || !sysinfo.Same(p, created) {
				continue
			}
			if err := sysinfo.SetCPUs(p, cpus); err == nil {
				j.applied[p] = key
			}
		}
		if j.container != "" && j.applied[-1] != key {
			j.applied[-1] = key
			if runtime.GOOS == "linux" {
				d.docker(j.engine, "update", "--cpuset-cpus", fmt.Sprintf("%d-%d", n-allow, n-1), j.container)
			} else { // Docker Desktop runs containers in a VM whose CPU numbers aren't the host's: cap the count
				d.docker(j.engine, "update", "--cpus", strconv.Itoa(allow), j.container)
			}
		}
	}
}

// heldForHuman: the human's state would hold this job anyway (a game holds the GPU or the network).
func (d *Daemon) heldForHuman(j *job) bool {
	return (j.GPU && d.gpuHeld()) || (j.Net && d.user == "game" && d.cfg.Network.PauseInGame)
}

// cpuWhy says when a job waits for CPUs ("" otherwise): its past runs keep NeedCPU CPUs busy, and
// with the jobs already running that passes what's outside the human's reserve (cpuRoom). A job
// with no history claims nothing, and nothing waits while no other job runs.
func (d *Daemon) cpuWhy(j *job) string {
	used := d.jobCPUs()
	if j.State == "running" || j.State == "granted" {
		used -= math.Max(j.cpuNow, j.NeedCPU) // not itself
	}
	if j.NeedCPU < 1 || used < 0.5 || used+j.NeedCPU <= d.cpuRoom() {
		return ""
	}
	return fmt.Sprintf("usually keeps %.1f CPUs busy; jobs already running use %.1f of the %.0f outside the human's reserve", j.NeedCPU, used, d.cpuRoom()/1.25)
}
