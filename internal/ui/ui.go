// Package ui is firefighter's look: magenta, hot pink, cream and ash on the terminal's own
// background, a diamond mark, and short stage-themed lines. Colors turn off for NO_COLOR, dumb
// terminals and non-terminals.
package ui

import (
	"fmt"
	"os"
	"strings"
)

type rgb struct{ r, g, b int }

var (
	Hot   = rgb{255, 79, 143}  // live states, errors
	Coral = rgb{226, 136, 131} // section headers, market lines
	Ivory = rgb{255, 243, 232} // waiting, present
	Ash   = rgb{140, 131, 121} // notes
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

const Banner = `  ___ _           __ _      _    _
 | __(_)_ _ ___  / _(_)__ _| |_ | |_ ___ _ _
 | _|| | '_/ -_)|  _| / _' | ' \|  _/ -_) '_|
 |_| |_|_| \___||_| |_\__, |_||_|\__\___|_|
                      |___/`

func PrintBanner() {
	for _, line := range strings.Split(Banner, "\n") {
		fmt.Fprintln(os.Stderr, Paint(Hot, line))
	}
	fmt.Fprintln(os.Stderr, Paint(Ash, "  your agents won't set your GPU on fire."))
}

// Mark starts every firefighter line.
const Mark = "◆"

// Say prints one status line to stderr behind the mark.
func Say(c rgb, format string, a ...any) {
	fmt.Fprintln(os.Stderr, Paint(c, Mark+" ")+Paint(c, fmt.Sprintf(format, a...)))
}

func Note(format string, a ...any) {
	fmt.Fprintln(os.Stderr, Paint(Ash, "   "+fmt.Sprintf(format, a...)))
}

// UserLabel names the human's state on stage.
func UserLabel(state string) string {
	switch state {
	case "game":
		return Paint(Hot, "on stage (game/media)")
	case "active":
		return Paint(Hot, "active")
	case "present":
		return Paint(Ivory, "present")
	}
	return Paint(Ash, "away")
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
