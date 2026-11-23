package main

// Configuration, setup, daemon lifecycle, updates, and reference commands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/setup"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
	"github.com/JoshWright22/firefighter/internal/ui"
	"github.com/JoshWright22/firefighter/internal/update"
)

// forecast prints what firefighter has learned: the human's use per state (and the reserve that
// follows from it), and each command's expected need on its next run.
func forecast(args []string) error {
	c, err := client.Connect()
	if err != nil {
		return err
	}
	s, err := c.Status()
	if err != nil {
		return err
	}
	f := s.Forecast
	if f == nil {
		f = &daemon.Forecast{}
	}
	if len(args) > 0 && args[0] == "--json" {
		b, _ := json.MarshalIndent(f, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	p := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	p("%s", ui.Paint(ui.Hot, "the human, by state (reserve in force; learned after a day of history in that state)"))
	p("  %-8s %-10s %-24s %-24s %s", "state", "history", "reserve", "configured", "seen: CPUs P95/mean, RAM and VRAM rise in 5 min (P95), net P95")
	for _, h := range f.Human {
		mark, src := " ", "configured"
		if h.Now {
			mark = "*"
		}
		if h.Learned {
			src = "learned"
		}
		p("%s %-8s %-10s %-24s %-24s %.1f/%.1f CPUs, +%.1f GB RAM, +%.1f GB VRAM, %.1f Mbps  (%s)", mark, h.State, fmt.Sprintf("%.1f days", h.Days),
			fmt.Sprintf("%d CPUs %.0f%% RAM %.1f GB", h.CPUs, h.RAMPct, h.VRAMGB), fmt.Sprintf("%d CPUs %.0f%% RAM %.1f GB", h.SetCPUs, h.SetRAMPct, h.SetVRAMGB),
			h.CPUP95, h.CPUMean, h.RAMRiseGB, h.VRAMRiseGB, h.NetMbps, src)
	}
	p("\n%s", ui.Paint(ui.Hot, "commands (expected on the next run; * = enough runs that history outranks the declaration)"))
	p("  %-14s %-34s %4s %8s %8s %6s %5s %8s %8s %8s", "project", "job", "runs", "RAM GB", "VRAM GB", "CPUs", "GPU%", "disk GB", "net Mbps", "minutes")
	for _, k := range f.Commands {
		warm := " "
		if k.Warm {
			warm = "*"
		}
		p("%s %-14s %-34s %4d %8.1f %8.1f %6.1f %5.0f %8.1f %8.1f %8.1f", warm, trunc(k.Project, 14), trunc(k.Name, 34), k.Runs, k.RAMGB, k.VRAMGB, k.CPUs, k.GPUPct, k.DiskGB, k.NetMbps, k.Minutes)
	}
	return nil
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
		if err := agentMayConfigure(args[1]); err != nil {
			return err
		}
		if err := config.Set(args[1], args[2]); err != nil {
			return err
		}
		ui.Say(ui.Hot, "%s = %s (the guard picks it up within a second)", args[1], args[2])
	case "unset":
		if len(args) != 2 {
			return errors.New("usage: firefighter config unset <dotted.key>")
		}
		if err := agentMayConfigure(args[1]); err != nil {
			return err
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
	ui.Note("new agent sessions will queue heavy work through FIREFIGHTER. Try: firefighter status")
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
	restarted := os.Getenv("FIREFIGHTER_RESTART") == "1" // started by the version before it, which is on its way out
	for i := 0; restarted && i < 75; i++ {
		if c, err := client.Connect0(); err != nil || c == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
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
	exe, _ := os.Executable() // now: after an update the running program's file is another one
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	unlock, err := daemon.Lock()
	for i := 0; err != nil && restarted && i < 75; i++ {
		time.Sleep(200 * time.Millisecond) // the version before this one is still letting go
		unlock, err = daemon.Lock()
	}
	if err != nil {
		return err
	}
	d := daemon.New(version)
	d.Exe = exe
	err = d.Serve()
	unlock()
	if err == nil && d.Restarting() {
		// Updated: come back as the new version. Under systemd or launchd a failed exit does that
		// (Restart=on-failure, KeepAlive on unsuccessful exit); anywhere else the new guard is
		// started from here.
		if os.Getenv("INVOCATION_ID") != "" || os.Getenv("XPC_SERVICE_NAME") == "dev.firefighter" {
			os.Exit(75)
		}
		return client.StartDaemonAt(exe, "FIREFIGHTER_RESTART=1")
	}
	return err
}

// updateCmd installs the newest release over this program and restarts the guard.
func updateCmd(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rel, err := update.Latest(ctx)
	if err != nil {
		return err
	}
	if version == "dev" {
		return fmt.Errorf("this is a development build, which never replaces itself; the newest release is %s (install: https://github.com/%s#install)", rel.Tag, update.Repo)
	}
	if !update.Newer(rel.Tag, version) {
		ui.Note("firefighter %s is the newest release", version)
		return nil
	}
	build := hasFlag(args, "--build")
	args = without(args, "--build")
	if len(args) > 0 && args[0] == "--check" {
		ui.Note("firefighter %s is out (this is %s): run `firefighter update`, or leave it to the guard", rel.Tag, version)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if err := update.Apply(ctx, rel, exe, func(s string) { ui.Note("%s", s) }, update.Options{Build: build}); err != nil {
		return err
	}
	_ = stopDaemon()
	return client.StartDaemonAt(exe, "FIREFIGHTER_RESTART=1")
}

const protocol = `# firefighter pause/checkpoint protocol for long GPU loops (Python; same idea in any language)
#
# The guard asks a job to pause while the human games, and to checkpoint and exit when the human
# needs its RAM or VRAM back. Jobs that honor it (and stop using the GPU while paused) are never
# suspended or killed mid-step; exit 75 after checkpointing and firefighter requeues the job.
import os, sys, time

# Its VRAM share on the card: cap the framework's cache to it at start, or the cache grows past the
# human's reserve and the job is stopped for it (FIREFIGHTER_VRAM_GB / FIREFIGHTER_VRAM_FRACTION).
if os.environ.get("FIREFIGHTER_VRAM_FRACTION"):
    try:
        import torch; torch.cuda.set_per_process_memory_fraction(float(os.environ["FIREFIGHTER_VRAM_FRACTION"]))
    except Exception:
        pass

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

// agentMayConfigure: from an agent's session, the CLI follows the same rule as the MCP tool (only
// order.*, pricing.* and gpu.*, and only when guard.agents_can_configure is on). A job held for RAM
// must not lead an agent to lower the human's reserve with one command (review round 6). A test's
// or sandbox's own state folder is its own to change. Not a sandbox: an agent running as the same
// user can still edit the file, but it can't do it by accident.
func agentMayConfigure(key string) error {
	if !config.IsDefaultHome() {
		return nil
	}
	c, _ := config.Load()
	if sysinfo.AgentAncestorPID(os.Getpid(), c.Guard.AgentParents) == 0 {
		return nil // the human's terminal
	}
	return agentConfigRule(key, c.Guard.AgentsCanConfigure)
}

// agentConfigRule is the agent-session rule itself; its refusal names the reason that applies.
// The reserve and packing need no agent: firefighter sizes them from measured use (docs/reserve.md).
func agentConfigRule(key string, allowed bool) error {
	switch {
	case allowed && config.AgentKey(key):
		return nil
	case !allowed:
		return fmt.Errorf("agents can't change settings here: guard.agents_can_configure is off, so every key, %s too, is the human's to set from their own terminal. firefighter already sizes the reserve and GPU packing from measured use (firefighter status shows what binds)", key)
	}
	return fmt.Errorf("%s is the human's setting: agents may change order.*, pricing.* and gpu.* only; ask the human", key)
}
