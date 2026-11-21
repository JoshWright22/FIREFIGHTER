package main

import (
	"io"
	"os"
	"sync"
)

// tailBuffer keeps the last max bytes written to it: the end of a job's output, sent with its exit
// code so the guard can tell why it failed (policy.ClassifyExit).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// teeIfPiped returns w copied into tail too, but only when w isn't a terminal: a job writing to a
// terminal keeps it (its progress bars, colors and line buffering), while one whose output already
// goes to a pipe or file (agents, detached jobs) loses nothing by passing through firefighter.
func teeIfPiped(w *os.File, tail *tailBuffer) io.Writer {
	if fi, err := w.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return w
	}
	return io.MultiWriter(w, tail)
}
