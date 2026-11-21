package daemon

// Container discovery, ownership, resource limits, and controls.

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// refreshDocker reads `docker stats` in the background every 15 s; a hung Docker never stalls the loop.
func (d *Daemon) refreshDocker(now time.Time) {
	if d.docking.Load() || now.Sub(d.dockerAt) < 15*time.Second {
		return
	}
	d.docking.Store(true)
	d.dockerAt = now
	// The docker/podman CLIs inside running jobs, read here (not under the lock): a container a
	// job's own script starts (run_matrix.py -> docker run) is that job's, not the human's.
	clis := map[string][]string{}   // job id -> the pids of its docker/podman CLIs
	d.mu.Lock()                     // called from the loop outside the lock
	for _, j := range d.running() { // adopted ones too: their containers' RAM is theirs either way
		for p := range j.pids {
			n := strings.TrimSuffix(strings.ToLower(sysinfo.Name(p)), ".exe")
			if n == "docker" || n == "podman" {
				clis[j.ID] = append(clis[j.ID], strconv.Itoa(p))
			}
		}
	}
	d.mu.Unlock()
	go func() {
		cs := sysinfo.Containers()
		lines := map[string][]string{}
		since := map[string]time.Time{} // job id -> when its first live docker run started
		for id, pids := range clis {
			for _, ps := range pids {
				p, _ := strconv.Atoi(ps)
				if cl := sysinfo.Cmdline(p); isDockerRun(cl) {
					lines[id] = append(lines[id], cl)
					if t := sysinfo.StartTime(p); !t.IsZero() && (since[id].IsZero() || t.Before(since[id])) {
						since[id] = t
					}
				}
			}
		}
		if len(cs) > 0 && len(clis) > 0 { // creation times and job labels, only while a job runs a docker CLI
			sysinfo.FillCreated(cs)
		}
		d.mu.Lock()
		d.others = cs
		d.adoptContainers(cs, lines, since)
		d.mu.Unlock()
		d.docking.Store(false)
	}()
}

// namedIn says whether a docker CLI line names its container exactly name (--name x / --name=x).
func namedIn(cl, name string) bool {
	f, ok := DockerRunOptions(cl) // the container's own options only: a --name after the image is the program's
	if !ok {
		return false
	}
	for i, a := range f {
		a = strings.Trim(a, `"'`)
		if (a == "--name" && i+1 < len(f) && strings.Trim(f[i+1], `"'`) == name) || a == "--name="+name {
			return true
		}
	}
	return false
}

// isDockerRun says whether a command line is `docker run` / `podman run` (or create).
func isDockerRun(cl string) bool {
	f := strings.Fields(strings.ToLower(cl))
	for i, a := range f {
		// Both separators on every OS: a Windows path (C:\Program Files\Docker\docker.exe) seen in a log or test on Linux.
		b := strings.Trim(strings.TrimSuffix(path.Base(strings.ReplaceAll(strings.Trim(a, `"`), `\`, "/")), ".exe"), `"`)
		if (b == "docker" || b == "podman") && i+1 < len(f) {
			for _, x := range f[i+1:] {
				if x == "run" || x == "create" {
					return true
				}
				if !strings.HasPrefix(x, "-") {
					return false
				}
			}
		}
	}
	return false
}

// adoptContainers gives each newly seen container to the running job whose own `docker run` CLI
// started it: the job's RAM then includes the container's (so the RAM ladder sees what the job
// really takes, inside the WSL VM), its pause and kill reach the container, and a container whose
// --memory is above the job's reservation is held to it (half again over the job's need, never
// under what it uses now). 2026-11-20: a job's script started `docker run --memory=16g`, the
// container counted as the human's load, and free RAM sat at 0.4 GB for five minutes with "the jobs'
// RAM is not what is missing".
func (d *Daemon) adoptContainers(cs []sysinfo.Container, lines map[string][]string, since map[string]time.Time) {
	// Without a --name match, a container is guessed to be a job's only when it is the one new
	// container in this scan, and never in the first scan after the guard starts: then every
	// container already running (the human's database, say) would be "new". (Review 11/20.)
	if d.seenCtr == nil {
		d.seenCtr = map[string]bool{}
	}
	seenBefore := map[string]bool{} // as it was before this scan: marking one container seen must not hide it from the next one's check
	for id := range d.seenCtr {
		seenBefore[id] = true
	}
	live := map[string]bool{}
	for _, c := range cs {
		live[c.ID] = true
		if d.seenCtr[c.ID] {
			continue
		}
		owned := false
		for _, j := range d.jobs {
			if (j.container != "" && strings.HasPrefix(j.container, c.ID)) || (j.ctrSeen != "" && strings.HasPrefix(j.ctrSeen, c.ID)) {
				owned = true
			}
		}
		if owned {
			d.seenCtr[c.ID] = true
			continue
		}
		// Labeled firefighter.job=<id> by the job's own script (FIREFIGHTER_JOB_ID): proven, the job's.
		if j := d.jobs[c.Job]; c.Job != "" && j != nil && j.State == "running" && j.container == "" {
			d.seenCtr[c.ID] = true
			j.container, j.engine, j.ctrSeen = c.ID, "docker", ""
			logf("%s: container %s (%s) is labeled firefighter.job=%s; it counts as the job's", j.Name, c.Name, c.ID, j.ID)
			// Held to the job's need when the job declared --ram (a guessed need could OOM-kill real
			// work); never under what it uses now. Its file cache counts toward the limit (11/20).
			if j.NeedRAM > 0 && j.RAMGB > 0 {
				lim := math.Max(j.NeedRAM, c.MemGB*1.2+0.5)
				m := fmt.Sprintf("%dm", int(lim*1024))
				d.docker("docker", "update", "--memory", m, "--memory-swap", m, c.ID)
				logf("%s: container %s limited to %.1f GB (the job declared %.1f GB)", j.Name, c.Name, lim, j.RAMGB)
			}
			continue
		}
		// The owner: the one job with a live `docker run`, or the one whose CLI names the container.
		var owner *job
		n, byName := 0, false
		for id, ls := range lines {
			j := d.jobs[id]
			if j == nil || j.State != "running" || len(ls) == 0 {
				continue
			}
			for _, l := range ls {
				if namedIn(l, c.Name) {
					owner, n, byName = j, 1, true
					break
				}
			}
			if n == 1 && owner == j {
				break
			}
			owner = j
			n++
		}
		if n == 0 {
			continue // no job's docker CLI seen yet (just after a restart, its processes aren't measured): look again
		}
		if n != 1 || owner == nil {
			d.seenCtr[c.ID] = true
			continue // two jobs could have started it: no guess
		}
		// The container must have been created after the job's docker run started (Review 11/20: the
		// human's database, already running, was otherwise guessed to be the job's and capped, paused
		// or killed with it). Without an exact --name match it must also be the only such container.
		start := since[owner.ID]
		if c.Created.IsZero() || start.IsZero() {
			continue // not known yet (docker inspect or the process start time missed): look again next scan
		}
		d.seenCtr[c.ID] = true
		if c.Created.Before(start.Add(-2 * time.Second)) {
			continue // older than the job's docker run: never the job's
		}
		if !byName {
			if start.IsZero() || c.Created.IsZero() {
				continue
			}
			others := 0
			for _, o := range cs {
				if o.ID != c.ID && !seenBefore[o.ID] && !o.Created.IsZero() && !o.Created.Before(start.Add(-2*time.Second)) {
					others++
				}
			}
			if others > 0 {
				continue // several new containers since then: no guess
			}
		}
		if owner.container != "" || owner.ctrSeen != "" {
			continue // a job's container is never replaced by another one
		}
		// Only the firefighter.job label (checked above) proves a container is the job's. A --name match
		// or the timing is a hint: the container's RAM counts toward the job's, but it is never paused,
		// capped or stopped with it (it could be the human's; review round 5, both reviewers).
		{
			owner.ctrSeen = c.ID
			how := "appeared while this job's docker run was live"
			if byName {
				how = "has the --name this job's docker run used"
			}
			logf("%s: container %s (%s) %s; its RAM counts toward the job's, but it is not paused, capped or stopped with it without the firefighter.job=%s label", owner.Name, c.Name, c.ID, how, owner.ID)
			continue
		}
	}
	for id := range d.seenCtr {
		if !live[id] {
			delete(d.seenCtr, id)
		}
	}

}

// underContainerRuntime says whether a process runs inside a container (a container runtime's
// shim is among its ancestors).
func underContainerRuntime(pid int) bool {
	for _, a := range sysinfo.Ancestors(pid) {
		n := strings.ToLower(sysinfo.Name(a))
		if strings.HasPrefix(n, "containerd-shim") || n == "conmon" || n == "crun" || n == "runc" {
			return true
		}
	}
	return false
}

// containerGPUJob says whether a GPU job is running in a container.
func (d *Daemon) containerGPUJob() bool {
	for _, j := range d.running() {
		if j.GPU && j.container != "" {
			return true
		}
	}
	return false
}

func (d *Daemon) signalContainer(j *job, verb string) {
	if j.container != "" {
		d.docker(j.engine, verb, j.container)
	}
}

// docker queues a container CLI call (engine first: docker or podman); calls run one at a time,
// in order.
func (d *Daemon) docker(engine string, args ...string) {
	if engine != "podman" {
		engine = "docker"
	}
	select {
	case d.dockerQ <- append([]string{engine}, args...):
	default:
		logf("container queue full; dropped: %s %s", engine, strings.Join(args, " "))
	}
}

func docker(engine string, args ...string) {
	// A hung engine (Docker Desktop restarting) must not block the queue behind it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, engine, args...)
	sysinfo.PrepareBackground(c)
	if out, err := c.CombinedOutput(); err != nil {
		logf("%s %s: %v %s", engine, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// containerFor says whether a container carries the label firefighter put on this job's docker run.
// containerFor returns the full id of the container name refers to, if it's labeled for job id
// ("" otherwise).
func containerFor(engine, name, id string) string {
	if strings.HasPrefix(name, "-") {
		return ""
	}
	if engine != "podman" {
		engine = "docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, engine, "inspect", "-f", `{{.Id}} {{index .Config.Labels "firefighter.job"}}`, name)
	sysinfo.PrepareBackground(c)
	out, err := c.Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) != 2 || f[1] != id {
		return ""
	}
	return f[0]
}

// dockerMemGB is the --memory (-m) limit of a `docker run` / `podman run` command line, in GB (0
// when there is none).
func dockerMemGB(command string) float64 {
	// Only the container's own options, up to the image: a --memory after it belongs to the
	// program inside (review 11/20).
	f, _ := DockerRunOptions(command)
	for i, a := range f {
		v := ""
		switch {
		case a == "--memory" || a == "-m":
			if i+1 < len(f) {
				v = f[i+1]
			}
		case strings.HasPrefix(a, "--memory="):
			v = strings.TrimPrefix(a, "--memory=")
		case strings.HasPrefix(a, "-m") && len(a) > 2 && a[2] >= '0' && a[2] <= '9':
			v = a[2:]
		}
		if v != "" {
			return memSize(v)
		}
	}
	return 0
}

// memSize parses docker's size syntax (512m, 28g, 1.5G, 1073741824) into GB.
func memSize(v string) float64 {
	v = strings.ToLower(strings.TrimSpace(v))
	mult := 1.0 / (1 << 30)
	switch {
	case strings.HasSuffix(v, "g"), strings.HasSuffix(v, "gb"):
		mult = 1
	case strings.HasSuffix(v, "m"), strings.HasSuffix(v, "mb"):
		mult = 1.0 / 1024
	case strings.HasSuffix(v, "k"), strings.HasSuffix(v, "kb"):
		mult = 1.0 / (1 << 20)
	case strings.HasSuffix(v, "b"):
		mult = 1.0 / (1 << 30) // bytes, as with no unit
	}
	n, err := strconv.ParseFloat(strings.TrimRight(v, "gmkb"), 64)
	if err != nil || !(n > 0) || math.IsInf(n, 0) || n*mult > 1e6 { // NaN, Inf or over a petabyte: not a real size
		return 0
	}
	return n * mult
}

// outsideContainers are the containers no job owns: the human's (or an agent's run outside firefighter).
func (d *Daemon) outsideContainers() []sysinfo.Container {
	var out []sysinfo.Container
	for _, c := range d.others {
		mine := false
		for _, j := range d.jobs {
			if j.container != "" && j.State != "done" && strings.HasPrefix(j.container, c.ID) {
				mine = true
			}
		}
		if !mine {
			out = append(out, c)
		}
	}
	return out
}
