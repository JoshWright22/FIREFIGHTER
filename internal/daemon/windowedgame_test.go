package daemon

import (
	"strings"
	"testing"
	"time"
)

// 45734113 (11/23): a nanoTSFM CUDA job was paused for "windowsterminal.exe using 20% of the 3D
// engine". A terminal or editor in front never counts as a game in a window, and any other app in
// front counts only once its 3D use has held for windowedGameAfter (a game holds it; a burst of
// rendering doesn't).
func TestWindowedGameNeedsSustained3DAndNotATerminal(t *testing.T) {
	d := testDaemon(t)
	now := time.Now()
	d.snap.ProcUtilKnown = true
	d.snap.Idle = 0

	d.snap.ForegroundApp = "WindowsTerminal.exe"
	d.graphics = map[string]float64{"windowsterminal.exe": 25}
	for i := 0; i < 4; i++ {
		d.senseUser(now.Add(time.Duration(i) * 15 * time.Second))
	}
	if strings.Contains(d.gameWhy, "3D engine") {
		t.Fatalf("a terminal in front counted as a game: %q", d.gameWhy)
	}

	d.gameWhy, d.gameTill, d.gameApp = "", time.Time{}, ""
	d.snap.ForegroundApp = "Game.exe"
	d.graphics = map[string]float64{"game.exe": 40}
	t0 := now.Add(time.Minute)
	d.senseUser(t0)
	if strings.Contains(d.gameWhy, "3D engine") {
		t.Fatalf("a first heavy reading counted at once: %q", d.gameWhy)
	}
	d.senseUser(t0.Add(25 * time.Second))
	if !strings.Contains(d.gameWhy, "game.exe using 40% of the 3D engine") {
		t.Fatalf("a game in a window held for 25 s didn't count: %q", d.gameWhy)
	}

	// A burst that stops before 20 s starts the clock over.
	d.gameWhy, d.gameTill, d.gameApp = "", time.Time{}, ""
	d.snap.ForegroundApp = "Editor.exe"
	d.graphics = map[string]float64{"editor.exe": 30}
	t1 := now.Add(5 * time.Minute)
	d.senseUser(t1)
	d.graphics = map[string]float64{"editor.exe": 2}
	d.senseUser(t1.Add(10 * time.Second))
	d.graphics = map[string]float64{"editor.exe": 30}
	d.senseUser(t1.Add(15 * time.Second))
	if strings.Contains(d.gameWhy, "3D engine") {
		t.Fatalf("separate bursts added up to a game: %q", d.gameWhy)
	}
}
