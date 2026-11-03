//go:build !windows

package sysinfo

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func hideWindow(*exec.Cmd) {}

func Suspend(pid int) error   { return send(pid, syscall.SIGSTOP) }
func Resume(pid int) error    { return send(pid, syscall.SIGCONT) }
func Kill(pid int) error      { return send(pid, syscall.SIGKILL) }
func Terminate(pid int) error { return send(pid, syscall.SIGTERM) }

// send never signals pid 0, 1, a negative pid ("my group", init, "everything") or itself.
func send(pid int, sig syscall.Signal) error {
	if pid <= 1 || pid == os.Getpid() {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	return syscall.Kill(-pgid(pid), sig)
}

// pgid addresses the job's whole process group when firefighter made that group (the pid leads it
// and its parent is a firefighter client); an adopted process that happens to lead a group (a
// shell pipeline) is signaled alone, so its siblings aren't hit.
func pgid(pid int) int {
	if g, err := syscall.Getpgid(pid); err == nil && g == pid {
		if a := Ancestors(pid); len(a) > 0 && strings.Contains(strings.ToLower(Name(a[0])), "firefighter") {
			return pid
		}
	}
	return -pid // negated again by the caller: a plain pid
}

// PrepareChild makes the child lead its own process group, so signals reach the whole tree. When
// the client is the terminal's foreground group, the child's group takes over the terminal, so
// prompts (input(), git, ssh, sudo) still read the keyboard instead of stopping on SIGTTIN.
func PrepareChild(cmd *exec.Cmd, _ []string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if fg, err := unix.IoctlGetInt(0, unix.TIOCGPGRP); err == nil && fg == syscall.Getpgrp() {
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = 0
	}
}

// PrepareBackground starts a helper (docker, the harvester) in its own group without ever taking
// the terminal: the daemon may run in one, and losing it would stop the daemon on SIGTTOU.
func PrepareBackground(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// RestoreForeground hands the terminal back to the client's group after a foreground child exits.
func RestoreForeground() {
	if _, err := unix.IoctlGetInt(0, unix.TIOCGPGRP); err != nil {
		return
	}
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, syscall.Getpgrp())
}

// Contain is a no-op on Unix: the process group plus the client's exit handler cover the tree.
func Contain(int) (func(), error) { return func() {}, nil }

func Detach(cmd *exec.Cmd, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func ExeName(n string) string { return n }

// Elevated says whether this process runs as root.
func Elevated() bool { return os.Geteuid() == 0 }
