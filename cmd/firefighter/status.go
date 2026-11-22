package main

// Machine status and human-readable display helpers.

import (
	"encoding/json"
	"fmt"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/ui"
)

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
	p("%s  human: %s %s", ui.Bold(ui.Paint(ui.Hot, "FIREFIGHTER")), ui.UserLabel(s.User), s.UserWhy)
	p("  reserve for the human: %.0f CPUs, %.1f GB RAM, %.1f GB VRAM (firefighter sizes it from measured use; config sets the bounds)", s.Reserve["cpus"], s.Machine["ram_reserve_gb"], s.Reserve["vram_gb"])
	if s.Forecast != nil {
		for _, h := range s.Forecast.Human {
			if h.Now && h.Learned {
				p("  (learned from %.1f days of your use while %s: %d CPUs, %.0f%% RAM, %.1f GB VRAM; configured %d, %.0f%%, %.1f GB. Details: firefighter forecast)",
					h.Days, h.State, h.CPUs, h.RAMPct, h.VRAMGB, h.SetCPUs, h.SetRAMPct, h.SetVRAMGB)
			}
		}
	}
	m := s.Machine
	p("  CPU %.1f/%.0f busy (%.1f human+other)  RAM %.1f/%.1f GB free  GPU %.0f%%  VRAM %.1f/%.1f GB",
		m["cpu_busy"], m["cpus"], m["user_cpus"], m["ram_avail_gb"], m["ram_total_gb"], m["gpu_util"], m["vram_used_gb"], m["vram_total_gb"])
	if s.VM != nil && s.VM.HostGB >= 1 {
		if s.VM.Guest {
			p("  WSL/Docker VM: holds %.1f GB of RAM, uses %.1f GB; %.1f GB is file cache FIREFIGHTER reclaims before touching a job", s.VM.HostGB, s.VM.UsedGB, s.VM.CacheGB)
		} else {
			p("  WSL/Docker VM: holds %.1f GB of RAM (its file cache is reclaimed when RAM runs short)", s.VM.HostGB)
		}
	}
	for _, dk := range s.Disks {
		p("  disk %s %.1f/%.1f GB free (%.0f GB kept for the human)", dk.Path, dk.FreeGB, dk.TotalGB, min(s.Reserve["disk_free_gb"], dk.TotalGB/10))
	}
	p("  policy: preset %s, order %s, pricing %s, budget %s", s.Policy["preset"], s.Policy["order"], s.Policy["pricing"], s.Policy["budget"])
	for _, l := range s.Limits {
		p("  %s", ui.Paint(ui.Ivory, "limit: "+l))
	}
	for _, l := range s.Idle {
		p("  %s", ui.Paint(ui.Hot, "! "+l))
	}
	for _, n := range s.Notes {
		p("  %s", ui.Paint(ui.Hot, "! "+n))
	}
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
		when := "starts " + daemon.Approx(v.ETA)
		if v.ETA == "" || v.ETA == "unknown" {
			when = "start not forecast"
		}
		p("  %-8s #%d %-31s %-14s %s: %s", v.ID, v.Position, trunc(v.Name, 31), trunc(v.Project, 14), when, v.Why)
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

func mins(m float64) string {
	if m < 1 {
		return "now"
	}
	if m >= 90 {
		return fmt.Sprintf("%.1fh", m/60)
	}
	return fmt.Sprintf("%.0fm", m)
}
