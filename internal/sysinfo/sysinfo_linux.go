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

func newPlatformReader() *platformReader             { return &platformReader{} }
func (*platformReader) procGPU() map[int]ProcGPU    { return nil }
func foregroundFullscreen() string                  { return "" }

// idleSeconds tries the desktop's idle monitor: GNOME/Mutter over D-Bus, then xprintidle (X11).
// Headless servers report unknown (-1), which counts as "present".
func idleSeconds() float64 {
	if out, err := run(time.Second, "gdbus", "call", "--session", "--dest", "org.gnome.Mutter.IdleMonitor",
		"--object-path", "/org/gnome/Mutter/IdleMonitor/Core", "--method", "org.gnome.Mutter.IdleMonitor.GetIdletime"); err == nil {
		s := strings.Trim(strings.TrimSpace(out), "()")
		s = strings.TrimPrefix(strings.TrimSuffix(s, ","), "uint64 ")
		if ms, err := strconv.ParseFloat(s, 64); err == nil {
			return ms / 1000
		}
	}
	if out, err := run(time.Second, "xprintidle"); err == nil {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err == nil {
			return ms / 1000
		}
	}
	return -1
}

// Background: nice 19, SCHED_IDLE, idle I/O class.
func Background(pid int) error {
	_ = unix.Setpriority(unix.PRIO_PROCESS, pid, 19)
	_ = unix.SchedSetAttr(pid, &unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: 5}, 0) // SCHED_IDLE
	const ioprioClassIdle = 3
	_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, 1, uintptr(pid), uintptr(ioprioClassIdle<<13))
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return err
	}
	return nil
}

func SetCPUs(pid int, cpus []int) error {
	var set unix.CPUSet
	for _, c := range cpus {
		set.Set(c)
	}
	if set.Count() == 0 {
		return nil
	}
	return unix.SchedSetaffinity(pid, &set)
}
