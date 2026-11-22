package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/harvest"
)

// harvestCmd shows the idle harvester's books, records what it earned, or lifts a trip.
//
//	firefighter harvest                         ledger totals, the guard's verdict now
//	firefighter harvest earned 3.20 [--through 2026-11-23] [--note "week 1"]
//	firefighter harvest reset                   allow it again after the guard tripped it off
//	firefighter harvest approve                 let the configured harvester commands run (the human, once per change)
func harvestCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p := cfg.Harvester.Profit
	p.Defaults()
	home := config.Home()
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "approve":
		// The human's own terminal only: approving is what lets these lines run at every logon.
		if err := agentMayConfigure("harvester.start"); err != nil {
			return fmt.Errorf("only the human approves harvester commands, from their own terminal")
		}
		h := cfg.Harvester
		cmds := []string{h.Start, h.Stop, h.Running}
		fmt.Println("approving these harvester commands from firefighter.toml (they run through the shell at logon and when the GPU goes idle):")
		for i, k := range []string{"start", "stop", "running"} {
			if strings.TrimSpace(cmds[i]) != "" {
				fmt.Printf("  %-8s %s\n", k, cmds[i])
			}
		}
		if err := harvest.Approve(home, cmds...); err != nil {
			return err
		}
		fmt.Println("approved; any later change to them needs `firefighter harvest approve` again")
		return nil
	case "earned":
		fs := flag.NewFlagSet("harvest earned", flag.ContinueOnError)
		through := fs.String("through", "", "last day (YYYY-MM-DD) or moment (YYYY-MM-DD HH:MM) the amount covers; default now")
		note := fs.String("note", "", "where the number came from")
		var amount string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			amount, args = args[0], args[1:]
		}
		if err := fs.Parse(args); err != nil {
			return err
		}
		if amount == "" && fs.NArg() > 0 {
			amount = fs.Arg(0)
		}
		usd, err := strconv.ParseFloat(strings.TrimPrefix(amount, "$"), 64)
		if err != nil || usd < 0 {
			return fmt.Errorf("usage: firefighter harvest earned <usd> [--through YYYY-MM-DD] [--note text]: the amount earned since the last report")
		}
		t := time.Now()
		if *through != "" {
			if t, err = parseDay(*through); err != nil {
				return err
			}
		}
		if err := harvest.AddEarning(home, harvest.Earning{Through: t, USD: usd, Note: *note}); err != nil {
			return err
		}
		fmt.Printf("recorded $%.2f earned through %s in %s\n", usd, t.Format("2006-01-02 15:04"), harvest.EarningsPath(home))
		return nil
	case "reset":
		if err := os.Remove(harvest.TripPath(home)); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("harvest guard reset: the harvester may run again when it pays (the daemon looks within a minute)")
		return nil
	case "", "show", "status":
	default:
		return fmt.Errorf("usage: firefighter harvest [earned <usd> [--through YYYY-MM-DD] [--note text] | reset]")
	}
	if cfg.Harvester.Start == "" {
		fmt.Println("no harvester configured ([harvester] start/stop in " + config.Path() + ")")
	}
	if !p.On() {
		fmt.Println("profit guard off: set harvester.profit.cents_per_kwh to price its power")
		return nil
	}
	b := harvest.Open(home)
	now := time.Now()
	show := func(label string, from time.Time) {
		t := b.Totals(p, from, now.Add(time.Minute))
		fmt.Printf("  %-12s %6.1f h  %6.2f kWh  power $%6.2f  earned $%6.2f  net $%6.2f", label, t.Hours, t.KWh, t.CostUSD, t.EarnedUSD,
			t.EarnedUSD*(1-p.CutPct/100)-t.CostUSD)
		if t.CoveredHours < t.Hours-0.01 {
			fmt.Printf("  (%.1f h not yet covered by an earnings report)", t.Hours-t.CoveredHours)
		}
		fmt.Println()
	}
	fmt.Printf("harvester books (%s, earnings after a %.0f%% cut):\n", home, p.CutPct)
	show("last 24 h", now.Add(-24*time.Hour))
	show("last 7 days", now.Add(-7*24*time.Hour))
	show("all time", time.Time{})
	fallback := 300.0
	c := b.Decide(p, now, harvest.WallWatts(p, fallback, 2))
	if trip, err := os.ReadFile(harvest.TripPath(home)); err == nil {
		fmt.Printf("guard: TRIPPED OFF: %s\n  firefighter harvest reset allows it again\n", strings.TrimSpace(string(trip)))
		return nil
	}
	verdict := "may run"
	if !c.OK {
		verdict = "held off"
	}
	src := "configured earn_per_hour"
	if c.Measured {
		src = "reported earnings"
	}
	fmt.Printf("guard now (%.1f¢/kWh): %s: %s [earnings rate from %s]\n", c.Cents, verdict, c.Why, src)
	fmt.Println("next 24 h by price:")
	last := -1.0
	for h := 0; h < 24; h++ {
		t := now.Truncate(time.Hour).Add(time.Duration(h) * time.Hour)
		if pr := p.Price(t); pr != last {
			cost := c.Watts / 1000 * pr / 100
			fmt.Printf("  from %s  %5.1f¢/kWh  power $%.3f/h  net $%+.3f/h\n", t.Format("Mon 15:04"), pr, cost, c.EarnHour-cost)
			last = pr
		}
	}
	return nil
}

func parseDay(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return t, fmt.Errorf("--through %q: use YYYY-MM-DD or \"YYYY-MM-DD HH:MM\"", s)
	}
	return t.Add(24*time.Hour - time.Minute), nil
}
