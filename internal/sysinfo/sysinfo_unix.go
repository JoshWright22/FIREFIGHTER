//go:build !windows

package sysinfo

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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

// pgid addresses the job's whole process group when the pid leads one, else just the pid.
func pgid(pid int) int {
	if g, err := syscall.Getpgid(pid); err == nil && g == pid {
		return pid
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
