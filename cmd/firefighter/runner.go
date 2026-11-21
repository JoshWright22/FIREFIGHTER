package main

// Granted turns: child lifecycle, GPU pinning, and receipts.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/ui"
)

// turn is one wait-run-receipt cycle. requeue is true when the guard evicted the job gracefully.
func turn(c *client.Client, t *policy.Ticket, argv []string, container string, quiet bool) (int, bool, error) {
	v, err := c.Create(daemon.Request{Ticket: *t})
	if errors.Is(err, client.ErrTaken) { // the same --request-id raced in from another client: never a second copy
		ui.Note("job %s is already queued by another client: waiting on it", t.ID)
		code, werr := wait([]string{t.ID})
		return code, false, werr
	}
	if err != nil {
		return 1, false, err
	}
	// Ctrl-C, a closed terminal, or an agent harness's SIGTERM: cancel at once, even mid-poll.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, stopSignals...)
	defer signal.Stop(interrupt)
	var cancelled atomic.Bool
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-interrupt:
			cancelled.Store(true)
			_, _ = c.Act(t.ID, "cancel", daemon.Act{})
		case <-waitDone:
		}
	}()

	// Print only news: a new place in line, or a start estimate that moved by over 20% (and 5 min),
	// so a waiting agent reading this output spends almost nothing.
	lastPos, lastStart, lastMarket := -1, -1.0, time.Time{}
	for v.State == "waiting" {
		moved := lastStart < 0 || math.Abs(v.StartsIn-lastStart) > math.Max(5, 0.2*lastStart)
		if !quiet && (v.Position != lastPos || moved) {
			when := daemon.Approx(v.ETA)
			if v.StartsIn >= 0 {
				when += " (in " + mins(v.StartsIn) + ")"
			}
			ui.Say(ui.Ivory, "%s %s waiting #%d, starts %s: %s", t.ID, t.Name, v.Position, when, v.Why)
			lastPos, lastStart = v.Position, v.StartsIn
			if v.Market != "" && time.Since(lastMarket) > 15*time.Minute {
				ui.Note("%s", v.Market)
				lastMarket = time.Now()
			}
		}
		if cancelled.Load() {
			return 130, false, errors.New("cancelled while waiting")
		}
		if v.Declared != nil { // amended or re-bid while waiting: re-register with those
			t.RAMGB, t.VRAMGB, t.Minutes, t.Bid, t.Spot = v.Declared.RAMGB, v.Declared.VRAMGB, v.Declared.Minutes, v.Declared.Bid, v.Declared.Spot
		}
		nv, err := c.Get(t.ID, "waiting", 20)
		if errors.Is(err, client.ErrNotFound) { // the daemon restarted: get back in line
			nv, err = c.Create(daemon.Request{Ticket: *t})
		}
		if err != nil {
			if c2, err2 := client.Connect(); err2 == nil {
				c = c2
				continue
			}
			return 1, false, err
		}
		v = nv
	}
	close(waitDone)
	if cancelled.Load() {
		return 130, false, errors.New("cancelled while waiting")
	}
	if v.State == "done" {
		if v.Receipt != nil && v.Receipt.Outcome == "cancelled" {
			return 130, false, errors.New("cancelled while waiting")
		}
		return 1, true, nil // closed under us (daemon restart): get back in line
	}
	if v.State != "granted" {
		// Only a granted turn starts the command (a job someone else marked running, or anything
		// unexpected, never makes it run): get back in line under a fresh attempt.
		if !quiet {
			ui.Note("the turn isn't ours (%s); back in line", v.State)
		}
		time.Sleep(time.Second)
		return 1, true, nil
	}
	if !quiet {
		ui.Say(ui.Hot, "%s: spotlight's yours (lease %.0f min, budget left %.1f)", t.Name, t.Minutes, v.Budget)
	}

	if container == "firefighter-"+t.ID {
		// A requeued attempt reuses the name: clear the previous attempt's stopped container.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Only a stopped one that carries this job's label, and no -f: a name alone proves nothing,
		// and a running container is never removed from here (review 11/20).
		ps := exec.CommandContext(ctx, argv[0], "ps", "-aq", "--filter", "label=firefighter.job="+t.ID,
			"--filter", "name=^/?"+container+"$", "--filter", "status=exited", "--filter", "status=created")
		sysinfo.PrepareBackground(ps)
		if out, err := ps.Output(); err == nil {
			if ids := strings.Fields(string(out)); len(ids) > 0 {
				rm := exec.CommandContext(ctx, argv[0], append([]string{"rm"}, ids...)...)
				sysinfo.PrepareBackground(rm)
				_ = rm.Run()
			}
		}
	}
	argv = dockerGPU(argv, v.Device, v.Vendor) // a docker --gpus job goes to the card it was admitted on
	cmd := exec.Command(argv[0], argv[1:]...)
	tail := &tailBuffer{max: 64 << 10}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, teeIfPiped(os.Stdout, tail), teeIfPiped(os.Stderr, tail)
	// The output may go through a pipe (teeIfPiped): without a delay, Wait would also wait for any
	// process the job left holding it (a build server, `server &`), and the job would stay open past
	// its lease (review 11/20). Its own exit is what ends the job.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(),
		"FIREFIGHTER_JOB_ID="+t.ID,
		"FIREFIGHTER_PAUSE_FILE="+daemon.PauseFile(t.ID),
		"FIREFIGHTER_PAUSED_FILE="+daemon.PausedFile(t.ID),
		"FIREFIGHTER_CHECKPOINT_FILE="+daemon.CheckpointFile(t.ID))
	// Multi-GPU: the daemon admitted the job against one card's free VRAM; run it there.
	cmd.Env = append(cmd.Env, pinGPU(v.Device, v.Vendor)...)
	sysinfo.PrepareChild(cmd, argv)
	defer sysinfo.RestoreForeground()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		_, _ = c.Act(t.ID, "done", daemon.Act{Exit: 127, Project: t.Project})
		return 127, false, err
	}
	release, cerr := sysinfo.Contain(cmd.Process.Pid)
	if cerr != nil && !quiet {
		// It still runs (failing here would break environments that can't nest job objects), but
		// what it starts may outlive it: say so rather than promise the whole tree (review 11/20).
		ui.Note("couldn't put %s in a job object (%v): processes it starts may outlive it", t.Name, cerr)
	}
	sysinfo.Started(cmd.Process.Pid) // Windows: it started suspended, so nothing it spawns escapes the job object
	var spare func(int) bool         // set once the job ended normally: build servers it started live on
	defer func() { release(spare) }()
	_, err = c.Act(t.ID, "start", daemon.Act{PID: cmd.Process.Pid})
	for try := 0; err != nil && try < 3 && !strings.Contains(err.Error(), "not granted"); try++ {
		time.Sleep(time.Second) // a transient failure (the guard couldn't tell who asked): ask again
		_, err = c.Act(t.ID, "start", daemon.Act{PID: cmd.Process.Pid})
	}
	if err != nil && strings.Contains(err.Error(), " is done,") {
		// Cancelled (or finished) while it was starting: that's final, not a lapsed turn.
		_ = sysinfo.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
		return 1, false, fmt.Errorf("job %s was cancelled before it started", t.ID)
	}
	if err != nil {
		if strings.Contains(err.Error(), "not granted") {
			// The turn lapsed (the machine slept between grant and start): don't run ungoverned;
			// stop the just-started command and get back in line.
			_ = sysinfo.Kill(cmd.Process.Pid)
			_ = cmd.Wait()
			if !quiet {
				ui.Note("the turn lapsed before the job started; back in line")
			}
			return 1, true, nil
		}
		// The guard didn't take it: it would run ungoverned (never paused for a game, never
		// stopped for the human) while still holding its turn. Stop it and give the turn back.
		_ = sysinfo.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
		_, _ = c.Act(t.ID, "cancel", daemon.Act{Project: t.Project})
		return 1, false, fmt.Errorf("the guard didn't accept the job's start (%v); not running it ungoverned", err)
	}
	attemptDone := make(chan struct{}) // closed when this attempt ends: a late registration can't reach the next one
	defer close(attemptDone)
	if container != "" {
		engine := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
		rc := c     // its own copy: the loop below may replace c on a reconnect (review 11/20: data race)
		go func() { // the container appears once its image is pulled: keep trying for 10 minutes
			for i := 0; i < 200; i++ {
				select {
				case <-attemptDone:
					return
				case <-time.After(3 * time.Second):
				}
				if _, err := rc.Act(t.ID, "container", daemon.Act{Container: container, Engine: engine}); err == nil {
					return
				}
			}
		}()
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	keepalive := time.NewTicker(5 * time.Second)
	var askedAt time.Time
	interrupted := false
	defer keepalive.Stop()
	var werr error
loop:
	for {
		select {
		case werr = <-done:
			break loop
		case <-interrupt:
			_ = sysinfo.Terminate(cmd.Process.Pid)
			if runtime.GOOS == "windows" {
				_ = sysinfo.Kill(cmd.Process.Pid)
			}
		case <-keepalive.C: // re-register if the daemon restarted, so the job stays governed
			cur, err := c.Get(t.ID, "", 0)
			if err == nil && cur.Checkpt && askedAt.IsZero() {
				// The human needs the machine back. Say why now; a job that follows the checkpoint
				// protocol saves and exits on its own (it's requeued under the same id).
				askedAt = time.Now()
				if !quiet {
					ui.Say(ui.Hot, "%s: FIREFIGHTER needs it to stop (%s); it gets time to save and exit, then it's requeued", t.Name, cur.Why)
				}
			}
			if err == nil && cur.Checkpt && !interrupted && runtime.GOOS != "windows" && cur.StopIn > 0 && cur.StopIn <= 10 {
				// Ten seconds before the hard stop and still running: it isn't checkpointing, so
				// SIGINT it (Ctrl+C: most programs exit cleanly) instead of letting SIGTERM/kill be
				// the first thing it hears. Not on Windows, where Ctrl+Break ends a program at once:
				// there it keeps its whole grace period.
				interrupted = true
				_ = sysinfo.Interrupt(cmd.Process.Pid)
			}
			if errors.Is(err, client.ErrNotFound) {
				t2 := *t // a copy: the running registration, not the ticket the next attempt queues with
				t2.PID = cmd.Process.Pid
				t2.Device, t2.Vendor = v.Device, v.Vendor // back on the same card
				_, _ = c.Create(daemon.Request{Ticket: t2, Running: true})
			} else if err != nil {
				if c2, err := client.Connect(); err == nil {
					c = c2
				}
			}
		}
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = exitCodeOf(ee)
	} else if errors.Is(werr, exec.ErrWaitDelay) {
		code = 0 // it exited ok; only something it left behind still held its output
	} else if werr != nil {
		code = 1
	}
	v, err = c.Act(t.ID, "done", daemon.Act{Exit: code, Project: t.Project, Tail: tail.String()})
	if err != nil {
		return code, false, nil
	}
	if cfg, err := config.Load(); err == nil {
		spare = func(p int) bool { return daemon.KeepAlive(cfg.Guard.KeepAlive, sysinfo.Name(p)) }
	}
	if rec := v.Receipt; rec != nil && !quiet {
		color := ui.Hot
		if rec.Outcome != "ok" {
			color = ui.Hot
		}
		ui.Say(color, "%s: curtain call, %s (exit %d) in %.1f min (declared %.0f), waited %.1f, charged %.1f machine-min, peak %.1f GB RAM / %.1f GB VRAM",
			t.Name, rec.Outcome, rec.Exit, time.Since(start).Minutes(), rec.Declared, rec.Waited, rec.Charged, rec.PeakRAM, rec.PeakVRAM)
		if rec.Requeue && rec.Reason != "" && askedAt.IsZero() {
			ui.Say(ui.Hot, "%s: stopped because %s", t.Name, rec.Reason)
		}
		if f := rec.Failure; f != nil {
			retry := map[string]string{"same": "running it again may work", "change": "change something before running it again", "no": "it won't work as it is"}[f.Retry]
			ui.Say(ui.Hot, "%s: failed because %s; %s | do: %s", t.Name, f.Why, retry, f.Fix)
		}
		if rec.Held > 0 {
			ui.Say(ui.Hot, "%s: suspended %d time(s), %.1f min in all, to free RAM or disk for the human (%.1f GB of RAM given back); it resumed where it stopped",
				t.Name, rec.Held, rec.HeldMinutes, rec.FreedRAM)
		}
		return code, rec.Requeue, nil
	}
	return code, v.Receipt != nil && v.Receipt.Requeue, nil
}

// pinGPU returns the environment that keeps a job on the card the daemon admitted it on, in that
// vendor's own numbering. Nothing when the user already chose devices, or the job wasn't placed.
func pinGPU(dev *int, vendor string) []string {
	if dev == nil {
		return nil
	}
	for _, k := range []string{"CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "ONEAPI_DEVICE_SELECTOR"} {
		if os.Getenv(k) != "" {
			return nil
		}
	}
	d := strconv.Itoa(*dev)
	switch vendor {
	case "nvidia":
		// nvidia-smi numbers cards by PCI bus; CUDA's default is fastest-first. Same order: PCI.
		return []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=" + d}
	case "amd":
		return []string{"HIP_VISIBLE_DEVICES=" + d, "ROCR_VISIBLE_DEVICES=" + d}
	case "intel":
		return []string{"ONEAPI_DEVICE_SELECTOR=level_zero:" + d}
	}
	return nil
}

// dockerGPU narrows `docker run --gpus all` (or a count) to the card the daemon placed the job on.
func dockerGPU(argv []string, dev *int, vendor string) []string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	if dev == nil || vendor != "nvidia" || (base != "docker" && base != "podman") || len(argv) < 2 || argv[1] != "run" {
		return argv
	}
	out := append([]string{}, argv...)
	want := fmt.Sprintf("device=%d", *dev) // one device: no comma, so no CSV quoting needed
	end := len(out)
	if k := daemon.DockerImageAt(out[2:]); k >= 0 {
		end = k + 2
	}
	for i, a := range out[:end] {
		switch {
		case a == "--gpus" && i+1 < len(out) && !strings.Contains(out[i+1], "device="):
			out[i+1] = want
		case strings.HasPrefix(a, "--gpus=") && !strings.Contains(a, "device="):
			out[i] = "--gpus=" + want
		}
	}
	return out
}
