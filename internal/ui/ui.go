// Package ui is firefighter's look: a stage-light palette (hot pink, flame orange, gold, violet) and
// short stage-themed lines. Colors turn off for NO_COLOR, dumb terminals and non-terminals.
package ui

import (
	"fmt"
	"os"
	"strings"
)

type rgb struct{ r, g, b int }

var (
	Pink   = rgb{255, 45, 149}
	Flame  = rgb{255, 106, 0}
	Gold   = rgb{255, 201, 74}
	Violet = rgb{157, 78, 255}
	Ash    = rgb{150, 140, 160}
	Ember  = rgb{255, 70, 70}
)

var color = detect()

func detect() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func Paint(c rgb, s string) string {
	if !color {
		return s
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", c.r, c.g, c.b, s)
}

func Bold(s string) string {
	if !color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// Gradient paints s from pink through flame to gold, one step per rune.
func Gradient(s string) string {
	if !color {
		return s
	}
	stops := []rgb{Pink, Flame, Gold}
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		t := 0.0
		if len(rs) > 1 {
			t = float64(i) / float64(len(rs)-1) * float64(len(stops)-1)
		}
		k := int(t)
		if k >= len(stops)-1 {
			k = len(stops) - 2
		}
		f := t - float64(k)
		a, z := stops[k], stops[k+1]
		mix := func(x, y int) int { return x + int(float64(y-x)*f) }
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm%c", mix(a.r, z.r), mix(a.g, z.g), mix(a.b, z.b), r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

const Banner = `  ___ _           __ _      _    _
 | __(_)_ _ ___  / _(_)__ _| |_ | |_ ___ _ _
 | _|| | '_/ -_)|  _| / _' | ' \|  _/ -_) '_|
 |_| |_|_| \___||_| |_\__, |_||_|\__\___|_|
                      |___/`

func PrintBanner() {
	for _, line := range strings.Split(Banner, "\n") {
		fmt.Fprintln(os.Stderr, Gradient(line))
	}
	fmt.Fprintln(os.Stderr, Paint(Ash, "  your agents start the fires. you stay the headliner."))
}

// Say prints one status line to stderr with a flame mark.
func Say(c rgb, format string, a ...any) {
	fmt.Fprintln(os.Stderr, Paint(c, "🔥 ")+Paint(c, fmt.Sprintf(format, a...)))
}

func Note(format string, a ...any) {
	fmt.Fprintln(os.Stderr, Paint(Ash, "   "+fmt.Sprintf(format, a...)))
}

// UserLabel names the human's state on stage.
func UserLabel(state string) string {
	switch state {
	case "game":
		return Paint(Ember, "on stage (game/media)")
	case "active":
		return Paint(Pink, "active")
	case "present":
		return Paint(Gold, "present")
	}
	return Paint(Violet, "away")
}

func BandLabel(b int) string {
	switch b {
	case 1:
		return "opener"
	case 2:
		return "main set"
	case 3:
		return "afterparty (spot)"
	}
	return fmt.Sprint(b)
}
