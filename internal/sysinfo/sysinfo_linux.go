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
// idleSource is the source that answered last (-1: none yet); it's tried first, and when none
// answers the search is retried only once a minute.
var idleSource, idleMissAt = -1, time.Time{}

func idleSeconds() float64 {
	sources := []func() (float64, bool){
		func() (float64, bool) {
			out, err := run(time.Second, "gdbus", "call", "--session", "--dest", "org.gnome.Mutter.IdleMonitor",
				"--object-path", "/org/gnome/Mutter/IdleMonitor/Core", "--method", "org.gnome.Mutter.IdleMonitor.GetIdletime")
			if err != nil {
				return 0, false
			}
			ms, ok := gvariantNumber(out)
			return ms / 1000, ok
		},
		func() (float64, bool) {
			out, err := run(time.Second, "gdbus", "call", "--session", "--dest", "org.freedesktop.ScreenSaver",
				"--object-path", "/org/freedesktop/ScreenSaver", "--method", "org.freedesktop.ScreenSaver.GetSessionIdleTime")
			if err != nil {
				return 0, false
			}
			ms, ok := gvariantNumber(out)
			return ms / 1000, ok
		},
		func() (float64, bool) {
			if os.Getenv("WAYLAND_DISPLAY") != "" {
				return 0, false
			}
			out, err := run(time.Second, "xprintidle")
			if err != nil {
				return 0, false
			}
			ms, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
			return ms / 1000, err == nil
		},
	}
	if idleSource >= 0 {
		if v, ok := sources[idleSource](); ok {
			return v
		}
		idleSource = -1
	}
	if time.Since(idleMissAt) < time.Minute {
		return -1
	}
	for i, f := range sources {
		if v, ok := f(); ok {
			idleSource = i
			return v
		}
	}
	idleMissAt = time.Now()
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
	if pid <= 1 || pid == os.Getpid() {
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
	if pid <= 1 || pid == os.Getpid() {
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

// gpus: GPUs beyond what vendor tools list. Linux: nvidia-smi, rocm-smi and sysfs cover it; macOS
// GPUs share system memory (admitted as RAM).
func (*platformReader) gpus(map[string]bool) []GPU { return nil }

// startTicks is a process's start time in clock ticks since boot (/proc/<pid>/stat field 22). Unlike
// a wall-clock start time it doesn't move when the clock is stepped (NTP, WSL resync, dual boot).
func startTicks(pid int) (int64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the name may hold spaces and parentheses
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, false
	}
	t, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return t + 1, true // never 0 ("gone")
}

// Packaged: Linux apps don't run in a redirected package context.
func Packaged() bool { return false }
