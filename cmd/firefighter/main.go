// firefighter: lets AI agents share one computer's GPU, CPU and RAM while the human always comes first.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
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
	"github.com/JoshWright22/firefighter/internal/mcp"
	"github.com/JoshWright22/firefighter/internal/policy"
	"github.com/JoshWright22/firefighter/internal/setup"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/ui"
)

var version = "dev"

const usage = `firefighter: your agents won't set your GPU on fire.

  firefighter run [--gpu] [--net] --minutes N [--vram GB] [--ram GB] [--urgent] [--name NAME] -- <command...>
                                 wait for a turn, run the command here, print a receipt
      --detach                   queue it and return a job id at once (then: firefighter wait <id>)
      --evict never|graceful|force   what may happen if the human needs resources back
      --spot                     leftovers only: cheapest, preempted first
  firefighter quote [--gpu] --minutes N [--vram GB] [--ram GB]
                                 the market: price now, start time and cost per bid, busy odds
  firefighter bid <id> <1-10>    change a waiting job's tier (5 normal, 10 urgent)
  firefighter wait <id>          block until a job ends; prints its receipt and log tail
  firefighter status [--json]    the human's reserve, what's running until when, the queue, budgets
  firefighter extend <id> <min>  lengthen a running job's lease
  firefighter cancel <id>        cancel a waiting or running job
  firefighter adopt <pid> [--gpu] [--name NAME] [--minutes N]   govern a process already running
  firefighter plan "<what>" --when "<when>" --minutes N [--gpu PCT] [--vram GB] [--ram GB] [--done]
  firefighter config [show | set <key> <value> | unset <key> | presets | path]
  firefighter protocol           the pause/checkpoint helper for long GPU loops
  firefighter statusline [--json]  one line for a status bar (Claude Code, VS Code, tmux, T3 Code)
  firefighter events             NDJSON stream of status changes, for editor plugins
  firefighter setup              start at logon + teach every agent CLI (MCP + global rules)
  firefighter uninstall          undo setup
  firefighter daemon [--stop]    the guard itself (setup and the CLI start it for you)
  firefighter mcp                MCP server on stdio (setup registers it)
  firefighter version
`

func main() {
	client.Version = version
	if len(os.Args) < 2 {
		ui.PrintBanner()
		fmt.Fprint(os.Stderr, "\n"+usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "run":
		code, err = run(args)
	case "wait":
		code, err = wait(args)
	case "status":
		err = status(args)
	case "extend":
		err = extend(args)
	case "quote":
		err = quote(args)
	case "statusline":
		err = statusline(args)
	case "events":
		err = events(args)
	case "bid":
		err = rebid(args)
	case "cancel":
		err = act(args, "cancel")
	case "adopt":
		err = adopt(args)
	case "plan":
		err = plan(args)
	case "config":
		err = configCmd(args)
	case "protocol":
		fmt.Print(protocol)
	case "setup", "install":
		err = install(args)
	case "uninstall":
		err = uninstall(args)
	case "daemon":
		err = daemonCmd(args)
	case "mcp":
		err = mcp.Serve(version)
	case "version", "--version", "-v":
		fmt.Println("firefighter", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	if err != nil {
		ui.Say(ui.Hot, "%v", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// ---- run --------------------------------------------------------------------------------------------

type runFlags struct {
	gpu, spot, detach, quiet, urgent, net bool
	minutes, vram, ram                    float64
	bid                                   int
	name, project, evict, vendor          string
	id, log                               string
}

func parseRun(args []string) (*runFlags, []string, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	r := &runFlags{}
	fs.BoolVar(&r.gpu, "gpu", false, "needs the GPU")
	fs.BoolVar(&r.spot, "spot", false, "leftovers only")
	fs.BoolVar(&r.detach, "detach", false, "return a job id at once")
	fs.BoolVar(&r.quiet, "quiet", false, "no status lines")
	fs.Float64Var(&r.minutes, "minutes", 0, "runtime estimate (lease)")
	fs.Float64Var(&r.vram, "vram", 0, "peak VRAM GB")
	fs.Float64Var(&r.ram, "ram", 0, "peak RAM GB")
	fs.IntVar(&r.bid, "bid", 5, "1-10; prefer --urgent (10) or the default (5)")
	fs.BoolVar(&r.urgent, "urgent", false, "the human is waiting on the result: starts sooner, costs 2x")
	fs.BoolVar(&r.net, "net", false, "network-heavy (downloads, uploads); known installers and downloads are detected")
	fs.StringVar(&r.vendor, "gpu-vendor", "", "GPUs the job can use: nvidia, amd, intel or any (default: NVIDIA if present)")
	fs.StringVar(&r.name, "name", "", "job name")
	fs.StringVar(&r.project, "project", "", "project (default: the git repo or folder name)")
	fs.StringVar(&r.evict, "evict", "graceful", "never | graceful | force")
	fs.StringVar(&r.id, "id", "", "job id (internal)")
	fs.StringVar(&r.log, "log", "", "log file (internal)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if fs.NArg() == 0 {
		return nil, nil, errors.New("nothing to run: firefighter run [flags] -- <command...>")
	}
	if r.urgent {
		r.bid = 10
	}
	switch strings.ToLower(r.vendor) {
	case "", "nvidia", "amd", "intel", "any":
	default:
		return r, nil, fmt.Errorf("--gpu-vendor must be nvidia, amd, intel or any (got %q)", r.vendor)
	}
	if r.evict != "never" && r.evict != "graceful" && r.evict != "force" {
		return nil, nil, fmt.Errorf("--evict must be never, graceful or force")
	}
	return r, fs.Args(), nil
}

func projectOf(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return filepath.Base(d)
		}
		if filepath.Dir(d) == d {
			return filepath.Base(dir)
		}
	}
}

// hashCmd names a command for learning its runtime and peaks: the program and its script, not
// its arguments. Shell wrappers (sh -c, cmd /c) are looked through, and docker runs are named by
// image, so unrelated commands in one project don't share a history.
func hashCmd(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if len(argv) >= 3 {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
		case "sh", "bash", "zsh", "cmd", "powershell", "pwsh":
			if f := strings.Fields(argv[len(argv)-1]); len(f) > 0 { // the inner command
				argv = f
			}
		}
	}
	// The program and the script or module it runs, by base name, so "python train.py" and
	// "python ./train.py" (or a full path) are one command to learn from, and "python -m pytest" and
	// "python -m torch.distributed.run train.py" are not.
	base := func(a string) string {
		return strings.TrimSuffix(strings.ToLower(filepath.Base(filepath.Clean(a))), ".exe")
	}
	parts := []string{base(argv[0])}
	for i := 1; i < len(argv) && len(parts) < 4; i++ {
		a := argv[i]
		if a == "-m" && i+1 < len(argv) {
			parts = append(parts, "-m", argv[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		parts = append(parts, base(a))
		if len(parts) == 2 || (len(parts) > 2 && parts[1] == "-m") {
			break
		}
	}
	key := strings.Join(parts, " ")
	if base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe"); (base == "docker" || base == "podman") && len(argv) > 1 && argv[1] == "run" {
		key = base + " run " + dockerImage(argv[2:])
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:6])
}

// dockerImage finds the image in `docker run` arguments: the first argument that isn't a flag
// or a flag's value.
func dockerImage(args []string) string {
	if i := dockerImageAt(args); i >= 0 {
		return args[i]
	}
	return ""
}

// dockerImageAt is the index of the image in `docker run` arguments (after "run"), -1 if none:
// everything after it belongs to the container's own command and is never rewritten.
func dockerImageAt(args []string) int {
	withValue := map[string]bool{"--name": true, "-e": true, "--env": true, "-v": true, "--volume": true, "-p": true, "--publish": true,
		"-w": true, "--workdir": true, "--gpus": true, "--label": true, "-l": true, "--network": true, "--entrypoint": true, "-u": true, "--user": true,
		"--shm-size": true, "--memory": true, "-m": true, "--cpus": true, "--mount": true, "--env-file": true, "--platform": true}
	// Options that never take a value; any other "--opt" without "=" takes the next argument (docker
	// has many more valued options than flags, so this errs toward finding the real image).
	flags := map[string]bool{"--detach": true, "--interactive": true, "--tty": true, "--rm": true, "--privileged": true,
		"--init": true, "--read-only": true, "--publish-all": true, "--no-healthcheck": true, "--oom-kill-disable": true,
		"--help": true, "--quiet": true, "--disable-content-trust": true, "--replace": true, "--sig-proxy": true}
	_ = withValue
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		case strings.HasPrefix(a, "--"):
			if !strings.Contains(a, "=") && !flags[a] {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Short options: a cluster of flags (-it, -d, -P, -q), possibly ending in one valued
			// option (-dp 8080:80, -itv /a:/b), or a valued option with its value attached (-p80:80).
			rest := a[1:]
			k := strings.IndexFunc(rest, func(c rune) bool { return !strings.ContainsRune("ditPq", c) })
			if k >= 0 && k == len(rest)-1 && !strings.Contains(a, "=") {
				i++ // the last letter takes the next argument
			}
		default:
			return i
		}
	}
	return -1
}

// detachArgs rebuilds the child's command line from the parsed flags (never by filtering raw
// args, which could also hit the job's own flags after --).
func detachArgs(r *runFlags, argv []string) []string {
	out := []string{"--minutes", fmt.Sprint(r.minutes), "--bid", fmt.Sprint(r.bid), "--evict", r.evict}
	if r.gpu {
		out = append(out, "--gpu")
	}
	if r.spot {
		out = append(out, "--spot")
	}
	if r.vendor != "" {
		out = append(out, "--gpu-vendor", r.vendor)
	}
	if r.net {
		out = append(out, "--net")
	}
	if r.vram > 0 {
		out = append(out, "--vram", fmt.Sprint(r.vram))
	}
	if r.ram > 0 {
		out = append(out, "--ram", fmt.Sprint(r.ram))
	}
	if r.name != "" {
		out = append(out, "--name", r.name)
	}
	if r.project != "" {
		out = append(out, "--project", r.project)
	}
	return append(append(out, "--"), argv...)
}

func run(args []string) (int, error) {
	r, argv, err := parseRun(args)
	if err != nil {
		return 2, err
	}
	if r.minutes <= 0 {
		ui.Say(ui.Ivory, "no --minutes given: assuming 30. Pass an honest estimate; it's your lease.")
		r.minutes = 30
	}
	if r.detach {
		cwd, _ := os.Getwd()
		id, log, err := client.Detached(detachArgs(r, argv), cwd)
		if err != nil {
			return 1, err
		}
		fmt.Println(id)
		ui.Note("queued as %s; output in %s; `firefighter wait %s` for the receipt", id, log, id)
		return 0, nil
	}
	c, err := client.Connect()
	if err != nil {
		return 1, err
	}
	cwd, _ := os.Getwd()
	if r.project == "" {
		r.project = projectOf(cwd)
	}
	if r.name == "" {
		r.name = r.project + " " + filepath.Base(argv[0])
	}
	if r.id == "" {
		r.id = daemon.NewID()
	}
	argv, container := dockerize(argv, r.id)
	t := policy.Ticket{ID: r.id, Project: r.project, Name: r.name, Cwd: cwd, Command: strings.Join(argv, " "), CmdHash: hashCmd(argv),
		GPU: r.gpu, VRAMGB: r.vram, RAMGB: r.ram, Minutes: r.minutes, Bid: r.bid, Evict: r.evict, Spot: r.spot, ClientPID: os.Getpid(), WantVendor: r.vendor, Net: r.net || netCommand(argv), NetAuto: !r.net && netCommand(argv)}
	for attempt := 1; ; attempt++ {
		code, requeue, err := turn(c, t, argv, container, r.quiet)
		if err != nil || !requeue {
			return code, err
		}
		ui.Say(ui.Coral, "stopped to give the human the machine back; back in line automatically (attempt %d). Nothing to do: it restarts when there's room", attempt+1)
	}
}

// turn is one wait-run-receipt cycle. requeue is true when the guard evicted the job gracefully.
func turn(c *client.Client, t policy.Ticket, argv []string, container string, quiet bool) (int, bool, error) {
	v, err := c.Create(daemon.Request{Ticket: t})
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
			ui.Say(ui.Ivory, "%s %s waiting #%d, starts ~%s (in %s): %s", t.ID, t.Name, v.Position, v.ETA, mins(v.StartsIn), v.Why)
			lastPos, lastStart = v.Position, v.StartsIn
			if v.Market != "" && time.Since(lastMarket) > 15*time.Minute {
				ui.Note("%s", v.Market)
				lastMarket = time.Now()
			}
		}
		if cancelled.Load() {
			return 130, false, errors.New("cancelled while waiting")
		}
		nv, err := c.Get(t.ID, "waiting", 20)
		if errors.Is(err, client.ErrNotFound) { // the daemon restarted: get back in line
			nv, err = c.Create(daemon.Request{Ticket: t})
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
		rm := exec.CommandContext(ctx, argv[0], "rm", "-f", container)
		sysinfo.PrepareBackground(rm)
		_ = rm.Run()
	}
	argv = dockerGPU(argv, v.Device, v.Vendor) // a docker --gpus job goes to the card it was admitted on
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
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
	release, _ := sysinfo.Contain(cmd.Process.Pid)
	var spare func(int) bool // set once the job ended normally: build servers it started live on
	defer func() { release(spare) }()
	_, err = c.Act(t.ID, "start", daemon.Act{PID: cmd.Process.Pid})
	for try := 0; err != nil && try < 3 && !strings.Contains(err.Error(), "not granted"); try++ {
		time.Sleep(time.Second) // a transient failure (the guard couldn't tell who asked): ask again
		_, err = c.Act(t.ID, "start", daemon.Act{PID: cmd.Process.Pid})
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
	if container != "" {
		engine := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
		go func() { // the container appears once its image is pulled: keep trying for 10 minutes
			for i := 0; i < 200; i++ {
				time.Sleep(3 * time.Second)
				if _, err := c.Act(t.ID, "container", daemon.Act{Container: container, Engine: engine}); err == nil {
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
					ui.Say(ui.Hot, "%s: firefighter needs it to stop (%s); it gets time to save and exit, then it's requeued", t.Name, cur.Why)
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
				t2 := t
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
		code = ee.ExitCode()
	} else if werr != nil {
		code = 1
	}
	v, err = c.Act(t.ID, "done", daemon.Act{Exit: code, Project: t.Project})
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
		return code, rec.Requeue, nil
	}
	return code, v.Receipt != nil && v.Receipt.Requeue, nil
}

// dockerize labels `docker run` / `podman run` so the guard can find, pause, pin and kill the container.
func dockerize(argv []string, id string) ([]string, string) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	if (base != "docker" && base != "podman") || len(argv) < 2 || argv[1] != "run" {
		return argv, ""
	}
	name := ""
	end := len(argv)
	if k := dockerImageAt(argv[2:]); k >= 0 {
		end = k + 2
	}
	for i, a := range argv[:end] {
		if a == "--name" && i+1 < len(argv) {
			name = argv[i+1]
		} else if strings.HasPrefix(a, "--name=") {
			name = strings.TrimPrefix(a, "--name=")
		}
	}
	extra := []string{"--label", "firefighter.job=" + id}
	if name == "" {
		name = "firefighter-" + id
		extra = append(extra, "--name", name)
	}
	out := append([]string{argv[0], "run"}, extra...)
	return append(out, argv[2:]...), name
}

// ---- other commands ---------------------------------------------------------------------------------

func wait(args []string) (int, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError) // exit: the job's exit code; 124 on --timeout
	timeout := fs.Float64("timeout", 0, "give up after this many seconds (0 = never)")
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id != "" && !validJobID(id) {
		return 2, fmt.Errorf("%q isn't a firefighter job id (8 hex characters)", id)
	}
	if id == "" {
		return 2, errors.New("usage: firefighter wait <id> [--timeout s]")
	}
	c, err := client.Connect()
	if err != nil {
		return 1, err
	}
	deadline := time.Now().Add(time.Duration(*timeout * float64(time.Second)))
	v, err := c.GetSoon(id)
	last, shownMarket := "", false
	// An evicted job requeues under the same id: keep waiting for the attempt that finishes.
	for err == nil && (v.State != "done" || (v.Receipt != nil && v.Receipt.Requeue)) {
		if v.State == "done" {
			time.Sleep(500 * time.Millisecond)
			v, err = c.GetSoon(id)
			if v.State == "done" && v.Receipt != nil && v.Receipt.Requeue && time.Since(v.Receipt.Ended) > 30*time.Second {
				break // the requeued attempt never came back (its client died)
			}
			continue
		}
		if line := fmt.Sprintf("%s: %s #%d ~%s %s", id, v.State, v.Position, v.ETA, v.Why); line != last {
			ui.Note("%s", line)
			last = line
		}
		if v.Market != "" && !shownMarket {
			ui.Note("%s", v.Market)
			shownMarket = true
		}
		poll := 25
		if *timeout > 0 {
			left := time.Until(deadline).Seconds()
			if left <= 0 {
				return 124, fmt.Errorf("still %s", v.State)
			}
			poll = int(math.Max(1, math.Min(25, math.Ceil(left))))
		}
		v, err = c.Get(id, v.State, poll)
	}
	if err != nil {
		return 1, err
	}
	b, _ := json.MarshalIndent(v.Receipt, "", "  ")
	fmt.Println(string(b))
	if tail := client.Tail(client.LogPath(id), 2000); tail != "" {
		fmt.Println("--- log tail ---\n" + tail)
	}
	if v.Receipt != nil {
		return v.Receipt.Exit, nil
	}
	return 0, nil
}

func status(args []string) error {
	c, err := client.Connect()
	if err != nil {
		return err
	}
	s, err := c.Status()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "--json" {
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	p := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	p("%s  human: %s %s", ui.Bold(ui.Paint(ui.Hot, "firefighter")), ui.UserLabel(s.User), s.UserWhy)
	p("  reserve for the human: %.0f CPUs, %.0f%% RAM, %.1f GB VRAM", s.Reserve["cpus"], s.Reserve["ram_pct"], s.Reserve["vram_gb"])
	m := s.Machine
	p("  CPU %.1f/%.0f busy (%.1f human+other)  RAM %.1f/%.1f GB free  GPU %.0f%%  VRAM %.1f/%.1f GB",
		m["cpu_busy"], m["cpus"], m["user_cpus"], m["ram_avail_gb"], m["ram_total_gb"], m["gpu_util"], m["vram_used_gb"], m["vram_total_gb"])
	p("  policy: preset %s, order %s, pricing %s, budget %s", s.Policy["preset"], s.Policy["order"], s.Policy["pricing"], s.Policy["budget"])
	for _, k := range []string{"gpu", "cpu"} {
		if q, ok := s.Market[k]; ok {
			p("  %s", ui.Paint(ui.Coral, q.Line()))
		}
	}
	if s.ConfigErr != "" {
		p("  %s", ui.Paint(ui.Hot, "config error: "+s.ConfigErr))
	}
	p("\n%s", ui.Paint(ui.Hot, "on stage"))
	for _, v := range s.Running {
		note := v.Why
		if v.Paused {
			note = "paused: " + note
		}
		p("  %-8s %-34s %-14s until ~%s %s", v.ID, trunc(v.Name, 34), trunc(v.Project, 14), v.ETA, note)
	}
	p("\n%s", ui.Paint(ui.Ivory, "in the wings"))
	for _, v := range s.Waiting {
		p("  %-8s #%d %-31s %-14s on ~%s %s", v.ID, v.Position, trunc(v.Name, 31), trunc(v.Project, 14), v.ETA, v.Why)
	}
	if len(s.Foreign) > 0 || len(s.Docker) > 0 {
		p("\n%s", ui.Paint(ui.Coral, "other load (counted as the human's)"))
		for n, u := range s.Foreign {
			p("  %-30s GPU %.0f%%", n, u)
		}
		for _, d := range s.Docker {
			p("  docker %-23s CPU %.0f%%  RAM %.1f GB", d.Name, d.CPU, d.MemGB)
		}
	}
	if len(s.Budgets) > 0 {
		p("\n%s", ui.Paint(ui.Coral, "budgets (machine-minutes left)"))
		for k, b := range s.Budgets {
			p("  %-24s %.1f", k, b)
		}
	}
	if len(s.Plans) > 0 {
		p("\n%s", ui.Paint(ui.Ash, "planned"))
		for _, pl := range s.Plans {
			p("  %s %s: %s, %s, ~%.0f min", pl.ID, pl.Project, pl.What, pl.When, pl.Minutes)
		}
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func quote(args []string) error {
	fs := flag.NewFlagSet("quote", flag.ContinueOnError)
	gpu := fs.Bool("gpu", false, "needs the GPU")
	minutes := fs.Float64("minutes", 30, "runtime estimate")
	urgent := fs.Bool("urgent", false, "price the urgent tier as the job's own")
	vram := fs.Float64("vram", 0, "VRAM GB")
	ram := fs.Float64("ram", 0, "RAM GB")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := client.Connect()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	q, err := c.Quote(*gpu, *urgent, *minutes, *vram, *ram, projectOf(cwd), hashCmd(append([]string{""}, fs.Args()...)[1:]))
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(q, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Println(q.Line())
	return nil
}

func rebid(args []string) error {
	force, args := hasFlag(args, "--force"), without(args, "--force")
	if len(args) != 2 {
		return errors.New("usage: firefighter bid <id> <1-10>")
	}
	b, err := strconv.Atoi(args[1])
	if err != nil || b < 1 || b > 10 {
		return errors.New("bid must be 1-10")
	}
	c, err := client.Connect()
	if err != nil {
		return err
	}
	v, err := c.Act(args[0], "bid", daemon.Act{Minutes: float64(b), Project: callerProject(), Force: force})
	if err == nil {
		ui.Say(ui.Ivory, "%s: bid %d, now #%d, on ~%s", v.Name, b, v.Position, v.ETA)
	}
	return err
}

func extend(args []string) error {
	force, args := hasFlag(args, "--force"), without(args, "--force")
	if len(args) != 2 {
		return errors.New("usage: firefighter extend <id> <minutes>")
	}
	m, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return err
	}
	c, err := client.Connect()
	if err != nil {
		return err
	}
	v, err := c.Act(args[0], "extend", daemon.Act{Minutes: m, Project: callerProject(), Force: force})
	if err == nil {
		ui.Say(ui.Hot, "%s: encore, lease now ends ~%s", v.Name, v.ETA)
	}
	return err
}

func act(args []string, verb string) error {
	force := hasFlag(args, "--force")
	var ids []string
	for _, a := range args {
		if a != "--force" {
			ids = append(ids, a)
		}
	}
	if len(ids) != 1 {
		return fmt.Errorf("usage: firefighter %s <id> [--force]", verb)
	}
	args = ids
	c, err := client.Connect()
	if err != nil {
		return err
	}
	_, err = c.Act(args[0], verb, daemon.Act{Project: callerProject(), Force: force})
	return err
}

func adopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	gpu := fs.Bool("gpu", false, "uses the GPU")
	name := fs.String("name", "", "name")
	minutes := fs.Float64("minutes", 60, "expected minutes left")
	force := fs.Bool("force", false, "adopt a process from outside this session (from tmux, screen or a plain console)")
	var pidArg string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pidArg, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if pidArg == "" && fs.NArg() > 0 {
		pidArg = fs.Arg(0)
	}
	pid, err := strconv.Atoi(pidArg)
	if err != nil || !sysinfo.Alive(pid) {
		return errors.New("usage: firefighter adopt <running pid> [--gpu] [--name NAME] [--minutes N]")
	}
	c, err := client.Connect()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if *name == "" {
		*name = sysinfo.Name(pid)
	}
	v, err := c.Create(daemon.Request{Running: true, Adopt: true, Force: *force, Ticket: policy.Ticket{Project: projectOf(cwd), Name: *name, GPU: *gpu, Minutes: *minutes,
		PID: pid, ClientPID: os.Getpid(), Bid: 5, Evict: "graceful", CmdHash: hashCmd([]string{sysinfo.Name(pid)})}})
	if err == nil {
		ui.Say(ui.Hot, "adopted pid %d as %s (%s)", pid, v.ID, v.Name)
	}
	return err
}

func plan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	when := fs.String("when", "now", "when")
	minutes := fs.Float64("minutes", 0, "minutes")
	gpu := fs.Float64("gpu", 0, "GPU %")
	vram := fs.Float64("vram", 0, "VRAM GB")
	ram := fs.Float64("ram", 0, "RAM GB")
	done := fs.Bool("done", false, "remove this project's plans (or the plan with this id)")
	project := fs.String("project", "", "project")
	var what string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		what, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if what == "" {
		what = strings.Join(fs.Args(), " ")
	}
	c, err := client.Connect()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if *project == "" {
		*project = projectOf(cwd)
	}
	if *done {
		key := *project
		if what != "" {
			key = what
		}
		return c.Unplan(key)
	}
	p, err := c.Plan(daemon.Plan{Project: *project, What: what, When: *when, Minutes: *minutes, GPUPct: *gpu, VRAMGB: *vram, RAMGB: *ram})
	if err == nil {
		ui.Say(ui.Ivory, "announced %s: %s (%s); remove with `firefighter plan --done`", p.ID, p.What, p.When)
	}
	return err
}

func configCmd(args []string) error {
	if len(args) == 0 || args[0] == "show" {
		c, err := config.Load()
		fmt.Print(c.Show())
		return err
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: firefighter config set <dotted.key> <value>")
		}
		if err := config.Set(args[1], args[2]); err != nil {
			return err
		}
		ui.Say(ui.Hot, "%s = %s (the guard picks it up within a second)", args[1], args[2])
	case "unset":
		if len(args) != 2 {
			return errors.New("usage: firefighter config unset <dotted.key>")
		}
		return config.Unset(args[1])
	case "presets":
		fmt.Println("balanced   default: researched policy (LAS fair share, priced bids, Karma budgets, dynamic reserve)")
		fmt.Println("gamer      bigger reserves, one GPU job at a time, nothing on the GPU during games")
		fmt.Println("throughput small reserves, up to 3 GPU jobs packed by learned VRAM")
		fmt.Println("fair       DRF order, free bids, daily budgets: a shared lab machine")
		fmt.Println("\nfirefighter config set preset <name>")
	case "path":
		fmt.Println(config.Path())
	default:
		return fmt.Errorf("unknown config action %q", args[0])
	}
	return nil
}

func install(args []string) error {
	if sysinfo.Elevated() && os.Getenv("FIREFIGHTER_ALLOW_ELEVATED") != "1" {
		return errors.New("run setup from a normal (not administrator/root) terminal: it edits your own agent configs, and an elevated setup would write them as admin. Containers/CI: FIREFIGHTER_ALLOW_ELEVATED=1")
	}
	if len(args) > 0 {
		return fmt.Errorf("setup takes no arguments (got %q); it installs autostart, MCP and agent rules", args[0])
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	ui.PrintBanner()
	fmt.Fprintln(os.Stderr)
	failed := 0
	for _, s := range setup.Install(exe) {
		if s.Err != nil {
			failed++
			ui.Say(ui.Hot, "%s: %v", s.What, s.Err)
		} else {
			ui.Say(ui.Hot, "%s", s.What)
		}
	}
	if _, err := client.Connect(); err != nil {
		ui.Say(ui.Hot, "guard: %v", err)
	} else {
		ui.Say(ui.Hot, "guard running")
	}
	ui.Note("new agent sessions will queue heavy work through firefighter. Try: firefighter status")
	if failed > 0 {
		return fmt.Errorf("%d setup step(s) failed", failed)
	}
	return nil
}

func uninstall(args []string) error {
	if sysinfo.Elevated() && os.Getenv("FIREFIGHTER_ALLOW_ELEVATED") != "1" {
		return errors.New("run uninstall from a normal (not administrator/root) terminal: it edits your own agent configs")
	}
	if len(args) > 0 {
		return fmt.Errorf("uninstall takes no arguments (got %q)", args[0])
	}
	for _, s := range setup.Uninstall() {
		if s.Err != nil {
			ui.Say(ui.Hot, "%s: %v", s.What, s.Err)
		} else {
			ui.Say(ui.Ash, "%s", s.What)
		}
	}
	_ = stopDaemon()
	ui.Note("state stays in %s; delete it to forget budgets and history", config.Home())
	return nil
}

// stopDaemon asks the daemon to shut down over its authenticated API; it kills only a process that
// is really a firefighter daemon (never whatever reused a stale pid).
func stopDaemon() error {
	if c, err := client.Connect0(); err == nil {
		if c.Shutdown() == nil {
			for i := 0; i < 30; i++ {
				time.Sleep(100 * time.Millisecond)
				if _, err := client.Connect0(); err != nil {
					return nil
				}
			}
		}
	}
	b, err := os.ReadFile(daemon.EndpointPath())
	if err != nil {
		return nil
	}
	var ep daemon.Endpoint
	if json.Unmarshal(b, &ep) != nil || ep.PID <= 1 || !sysinfo.Alive(ep.PID) || !strings.Contains(sysinfo.Name(ep.PID), "firefighter") ||
		(ep.Created != 0 && !sysinfo.Same(ep.PID, ep.Created)) {
		return nil
	}
	return sysinfo.Kill(ep.PID)
}

func daemonCmd(args []string) error {
	if len(args) > 0 && args[0] == "--stop" {
		return stopDaemon()
	}
	if c, err := client.Connect0(); err == nil && c != nil {
		// Not an error: service managers (systemd Restart=on-failure, launchd) would otherwise
		// crash-loop when a CLI call already started the daemon on demand.
		ui.Note("the guard is already running")
		return nil
	}
	if sysinfo.Elevated() && os.Getenv("FIREFIGHTER_ALLOW_ELEVATED") != "1" {
		// Any agent of this user can read the token and write firefighter.toml: an elevated daemon
		// would act on their behalf with admin/root rights. It starts unelevated at your next logon.
		return errors.New("the guard won't run as administrator/root (it would lend those rights to every agent); it starts normally at your next logon, or run any firefighter command from a normal terminal. Containers/CI: FIREFIGHTER_ALLOW_ELEVATED=1")
	}
	unlock, err := daemon.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return daemon.New(version).Serve()
}

const protocol = `# firefighter pause/checkpoint protocol for long GPU loops (Python; same idea in any language)
#
# The guard asks a job to pause while the human games, and to checkpoint and exit when the human
# needs its RAM or VRAM back. Jobs that honor it (and stop using the GPU while paused) are never
# suspended or killed mid-step; exit 75 after checkpointing and firefighter requeues the job.
import os, sys, time

def firefighter_step(save_checkpoint=None):
    """Call once per training step."""
    pause = os.environ.get("FIREFIGHTER_PAUSE_FILE")
    if pause and os.path.exists(pause):
        ack = os.environ["FIREFIGHTER_PAUSED_FILE"]
        open(ack, "w").close()
        try:
            import torch; torch.cuda.empty_cache()
        except Exception:
            pass
        ckpt = os.environ.get("FIREFIGHTER_CHECKPOINT_FILE")
        while os.path.exists(pause) and not (ckpt and os.path.exists(ckpt)):
            time.sleep(1)
        if os.path.exists(ack):
            os.remove(ack)
    ckpt = os.environ.get("FIREFIGHTER_CHECKPOINT_FILE")
    if ckpt and os.path.exists(ckpt):
        if save_checkpoint:
            save_checkpoint()
        sys.exit(75)
`

func mins(m float64) string {
	if m < 1 {
		return "now"
	}
	if m >= 90 {
		return fmt.Sprintf("%.1fh", m/60)
	}
	return fmt.Sprintf("%.0fm", m)
}

// callerProject is the project of the current directory, sent so firefighter can refuse touching
// another project's job without --force.
func callerProject() string {
	wd, _ := os.Getwd()
	return projectOf(wd)
}

func without(args []string, f string) []string {
	var out []string
	for _, a := range args {
		if a != f {
			out = append(out, a)
		}
	}
	return out
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
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
	if k := dockerImageAt(out[2:]); k >= 0 {
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

func validJobID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// netCommand recognizes commands that are mostly network traffic: image pulls, package installs,
// clones, downloads. (`firefighter config set network.auto_detect false` turns this off; the
// daemon ignores the flag then.)
func netCommand(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	text := strings.Join(argv, " ")
	if len(argv) >= 3 {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
		case "sh", "bash", "zsh", "cmd", "powershell", "pwsh":
			text = argv[len(argv)-1]
		}
	}
	// Each step of a compound command ("cd x && pip install -r r.txt").
	for _, seg := range strings.FieldsFunc(text, func(r rune) bool { return r == '&' || r == ';' || r == '|' }) {
		if netStep(strings.Fields(seg)) {
			return true
		}
	}
	return false
}

func netStep(words []string) bool {
	if len(words) == 0 {
		return false
	}
	prog := strings.TrimSuffix(strings.ToLower(filepath.Base(words[0])), ".exe")
	has := func(subs ...string) bool { // a subcommand anywhere after options (pip --index-url U install)
		for _, w := range words[1:] {
			for _, s := range subs {
				if strings.EqualFold(w, s) {
					return true
				}
			}
		}
		return false
	}
	switch prog {
	case "curl", "wget", "aria2c", "rsync", "scp", "huggingface-cli", "hf", "gsutil", "rclone", "azcopy":
		return true
	case "aws":
		return has("s3") && has("cp", "sync", "mv")
	case "docker", "podman":
		return has("pull", "push") // build and compose up are mostly CPU
	case "git":
		return has("clone", "fetch", "pull", "lfs")
	case "pip", "pip3", "pipx", "npm", "pnpm", "yarn", "bun", "poetry", "conda", "mamba", "micromamba", "brew", "winget",
		"choco", "scoop", "apt", "apt-get", "dnf", "ollama":
		return has("install", "i", "ci", "add", "pull", "update", "upgrade", "sync", "create")
	case "cargo":
		return has("fetch") // install compiles: CPU-bound
	case "uv":
		return has("pip", "sync", "add")
	case "go":
		return has("get", "download")
	case "python", "python3", "py":
		return has("pip") && has("install") || strings.Contains(strings.Join(words, " "), "huggingface_hub")
	}
	return false
}
