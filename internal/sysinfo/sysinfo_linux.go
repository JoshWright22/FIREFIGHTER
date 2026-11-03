//go:build linux

package sysinfo

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type platformReader struct{}

func newPlatformReader() *platformReader         { return &platformReader{} }
func (*platformReader) procGPU() map[int]ProcGPU { return nil }
func foregroundFullscreen() string               { return "" }
func foregroundApp() string                      { return "" }

// idleSeconds asks the desktop: GNOME/Mutter, then the freedesktop ScreenSaver interface (KDE and
// most Wayland desktops), then xprintidle (X11). Headless machines report unknown (-1), which
// counts as "present".
func idleSeconds() float64 {
	if out, err := run(time.Second, "gdbus", "call", "--session", "--dest", "org.gnome.Mutter.IdleMonitor",
		"--object-path", "/org/gnome/Mutter/IdleMonitor/Core", "--method", "org.gnome.Mutter.IdleMonitor.GetIdletime"); err == nil {
		if ms, ok := gvariantNumber(out); ok {
			return ms / 1000
		}
	}
	if out, err := run(time.Second, "gdbus", "call", "--session", "--dest", "org.freedesktop.ScreenSaver",
		"--object-path", "/org/freedesktop/ScreenSaver", "--method", "org.freedesktop.ScreenSaver.GetSessionIdleTime"); err == nil {
		if ms, ok := gvariantNumber(out); ok {
			return ms / 1000
		}
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		if out, err := run(time.Second, "xprintidle"); err == nil {
			if ms, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err == nil {
				return ms / 1000
			}
		}
	}
	return -1
}

// gvariantNumber parses "(uint64 1234,)" or "(1234,)".
func gvariantNumber(out string) (float64, bool) {
	s := strings.Trim(strings.TrimSpace(out), "()")
	s = strings.TrimSuffix(s, ",")
	if i := strings.LastIndex(s, " "); i >= 0 {
		s = s[i+1:]
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// threads lists every thread of a process: Linux priority, scheduling class, I/O class and
// affinity are per thread, and PyTorch/OpenMP/JVM workers already exist by the time we see them.
func threads(pid int) []int {
	ents, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return []int{pid}
	}
	out := make([]int, 0, len(ents))
	for _, e := range ents {
		if t, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// Background: nice 19, SCHED_IDLE and the idle I/O class, on every thread.
func Background(pid int) error {
	if pid <= 1 {
		return os.ErrInvalid
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return err
	}
	const ioprioClassIdle = 3
	for _, t := range threads(pid) {
		_ = unix.Setpriority(unix.PRIO_PROCESS, t, 19)
		_ = unix.SchedSetAttr(t, &unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: 5}, 0) // SCHED_IDLE
		_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, 1, uintptr(t), uintptr(ioprioClassIdle<<13))
	}
	return nil
}

// SetCPUs pins every thread of a process to the given CPUs.
func SetCPUs(pid int, cpus []int) error {
	if pid <= 1 {
		return os.ErrInvalid
	}
	var set unix.CPUSet
	for _, c := range cpus {
		set.Set(c)
	}
	if set.Count() == 0 {
		return nil
	}
	var err error
	for _, t := range threads(pid) {
		if e := unix.SchedSetaffinity(t, &set); e != nil {
			err = e
		}
	}
	return err
}
