package main

// Editor integration: one-line status for any status bar (Claude Code statusLine, VS Code, tmux,
// starship, T3 Code) and an NDJSON event stream any plugin can subscribe to.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/daemon"
)

// Line is the compact status an editor shows.
type Line struct {
	Text      string  `json:"text"`
	User      string  `json:"user"`
	Running   int     `json:"running"`
	Waiting   int     `json:"waiting"`
	Price     float64 `json:"gpu_price"`
	Busy60    float64 `json:"gpu_busy_60m"`
	NextStart string  `json:"next_start,omitempty"`
	Tooltip   string  `json:"tooltip"`
}

func lineOf(s daemon.Status) Line {
	l := Line{User: s.User, Running: len(s.Running), Waiting: len(s.Waiting)}
	q, ok := s.Market["gpu"]
	if ok {
		l.Price, l.Busy60 = q.Price, q.Busy["60m"]
	}
	if len(s.Waiting) > 0 {
		l.NextStart = s.Waiting[0].ETA
	}
	icon := map[string]string{"game": "🎮", "active": "⌨", "present": "👀", "away": "💤"}[s.User]
	l.Text = fmt.Sprintf("◆ %s %d on stage · %d waiting · GPU x%.2f · busy 1h %.0f%%", icon, l.Running, l.Waiting, l.Price, 100*l.Busy60)
	var tip []string
	tip = append(tip, fmt.Sprintf("You: %s. Reserve %.0f CPUs, %.0f%% RAM, %.1f GB VRAM.", s.User, s.Reserve["cpus"], s.Reserve["ram_pct"], s.Reserve["vram_gb"]))
	for _, v := range s.Running {
		tip = append(tip, fmt.Sprintf("▶ %s (%s) until ~%s", v.Name, v.Project, v.ETA))
	}
	for _, v := range s.Waiting {
		tip = append(tip, fmt.Sprintf("… #%d %s (%s) on ~%s: %s", v.Position, v.Name, v.Project, v.ETA, v.Why))
	}
	if ok {
		tip = append(tip, q.Line())
	}
	l.Tooltip = strings.Join(tip, "\n")
	return l
}

// statusline prints one line (or --json). It never starts the daemon and never fails loudly:
// status bars call it every few seconds.
func statusline(args []string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	c, err := client.Connect0()
	if err != nil {
		if asJSON {
			fmt.Println(`{"text":"◆ off","user":"","tooltip":"firefighter isn't running: run firefighter status"}`)
		} else {
			fmt.Println("◆ off")
		}
		return nil
	}
	s, err := c.Status()
	if err != nil {
		fmt.Println("◆ ?")
		return nil
	}
	l := lineOf(s)
	if asJSON {
		b, _ := json.Marshal(l)
		fmt.Println(string(b))
		return nil
	}
	fmt.Println(l.Text)
	return nil
}

// events streams one JSON object per line whenever the picture changes (and a heartbeat every
// 30 s), until stdin closes or the process is killed. Editors spawn it once and read lines.
func events(args []string) error {
	c, err := client.Connect0()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	last, beat := "", time.Now()
	for {
		s, err := c.Status()
		if err != nil {
			if c2, err2 := client.Connect0(); err2 == nil {
				c = c2
			}
			time.Sleep(2 * time.Second)
			continue
		}
		l := lineOf(s)
		key := l.Text + l.Tooltip
		if key != last || time.Since(beat) > 30*time.Second {
			out := map[string]any{"type": "status", "time": s.Time, "line": l, "status": s}
			if err := enc.Encode(out); err != nil {
				return nil // reader went away
			}
			last, beat = key, time.Now()
		}
		time.Sleep(2 * time.Second)
	}
}
