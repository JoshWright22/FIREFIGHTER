package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/setup"
	"github.com/JoshWright22/firefighter/internal/storage"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// doctor checks each part of the install and prints the fix for each one that isn't right (idea
// from dsh-local-orchestrator's `doctor`): one line per check, exit 1 if anything needs fixing.
func doctor(args []string) (int, error) {
	bad := 0
	ok := func(what, detail string) { fmt.Printf("  ok    %-14s %s\n", what, detail) }
	fix := func(what, detail, how string) {
		bad++
		fmt.Printf("  FIX   %-14s %s\n                       do: %s\n", what, detail, how)
	}
	note := func(what, detail string) { fmt.Printf("  note  %-14s %s\n", what, detail) }
	fmt.Println("firefighter doctor")

	// The program itself, and the one agents find on PATH.
	exe, _ := os.Executable()
	if p, err := exec.LookPath("firefighter"); err != nil {
		fix("PATH", "firefighter isn't on PATH, so agents can't run it", "run the installer again, or add "+filepath.Dir(exe)+" to PATH")
	} else if a, b := filepath.Clean(p), filepath.Clean(exe); !strings.EqualFold(a, b) {
		note("PATH", "agents run "+a+"; this is "+b)
	} else {
		ok("PATH", p)
	}
	if version == "dev" {
		note("version", "a development build (go install or a local build): it doesn't update itself")
	} else {
		ok("version", version)
	}

	// Settings.
	cfg, err := config.Load()
	if err != nil {
		fix("config", err.Error(), "fix the file, or `firefighter config unset <key>` for the bad key")
	} else {
		ok("config", config.Path())
	}

	// The guard.
	c, err := client.Connect0() // look only: never start, stop or replace the guard while diagnosing it
	if err != nil {
		fix("guard", "not running ("+err.Error()+")", "any firefighter command starts it (setup also starts it at logon)")
	} else if st, err := c.Status(); err != nil {
		fix("guard", "not answering: "+err.Error(), "firefighter daemon --stop, then any firefighter command starts it again")
	} else {
		if st.Version != version && version != "dev" {
			fix("guard", "running "+st.Version+", this program is "+version, "firefighter daemon --stop (the next command starts the new one)")
		} else {
			ok("guard", "running "+st.Version+", the human is "+st.User)
		}
		if st.ConfigErr != "" {
			fix("config", "the guard can't use the settings: "+st.ConfigErr, "fix "+config.Path())
		}
		if len(st.GPUs) == 0 {
			note("GPU", "no discrete graphics card found (GPU jobs are admitted against shared memory, if the machine has any)")
		} else {
			for _, g := range st.GPUs {
				ok("GPU", fmt.Sprintf("%s %s, %.1f/%.1f GB VRAM free", g.Vendor, g.Name, g.VRAMGB-g.UsedGB, g.VRAMGB))
			}
		}
		for _, dk := range st.Disks {
			floor := min(st.Reserve["disk_free_gb"], dk.TotalGB/10)
			if dk.FreeGB < floor {
				fix("disk", fmt.Sprintf("%s has %.1f GB free, under the %.0f GB kept for the human: no job starts there", dk.Path, dk.FreeGB, floor), "free space on "+dk.Path)
			} else {
				ok("disk", fmt.Sprintf("%s %.1f GB free", dk.Path, dk.FreeGB))
			}
		}
		if len(st.Notes) > 0 {
			for _, n := range st.Notes {
				note("now", n)
			}
		}
	}

	// What each agent was taught.
	for _, r := range setup.Rules() {
		switch r.State {
		case "current":
			ok("agent rules", r.Path)
		case "outdated":
			fix("agent rules", r.Path+" has an older firefighter block", "firefighter setup")
		default:
			fix("agent rules", r.Path+" has no firefighter block, so that agent doesn't know to queue", "firefighter setup")
		}
	}

	// Docker, when it's installed.
	if _, err := exec.LookPath("docker"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		sysinfo.PrepareBackground(cmd)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			note("docker", "installed but the engine isn't answering (container jobs will fail until it runs)")
		} else {
			ok("docker", "engine "+strings.TrimSpace(string(out)))
		}
	}

	// Agent storage.
	{
		if cfg.Storage.GB <= 0 {
			note("storage", "none allotted to agents (`firefighter config set storage.gb <GB>` gives them space)")
		} else if r, err := storage.FromConfig(cfg).Look(config.Home()); err != nil {
			fix("storage", err.Error(), "check "+filepath.Join(config.Home(), "storage.json"))
		} else {
			over := 0
			for _, s := range r.Spaces {
				if s.Over {
					over++
				}
			}
			if over > 0 {
				note("storage", fmt.Sprintf("%d space(s) over their claim: those projects' jobs wait (firefighter storage)", over))
			} else {
				ok("storage", fmt.Sprintf("%.1f of %.1f GB claimed at %s", r.GrantedGB, r.GB, r.Root))
			}
		}
	}

	if bad > 0 {
		fmt.Printf("%d thing(s) to fix\n", bad)
		return 1, nil
	}
	fmt.Println("all good")
	return 0, nil
}
