//go:build !windows

package sysinfo

import (
	"os/exec"
	"syscall"
)

func hideWindow(*exec.Cmd) {}

func Suspend(pid int) error   { return syscall.Kill(-pgid(pid), syscall.SIGSTOP) }
func Resume(pid int) error    { return syscall.Kill(-pgid(pid), syscall.SIGCONT) }
func Kill(pid int) error      { return syscall.Kill(-pgid(pid), syscall.SIGKILL) }
func Terminate(pid int) error { return syscall.Kill(-pgid(pid), syscall.SIGTERM) }

// pgid addresses the job's whole process group when the pid leads one, else just the pid.
func pgid(pid int) int {
	if g, err := syscall.Getpgid(pid); err == nil && g == pid {
		return pid
	}
	return -pid // negated again by the caller: a plain pid
}

// PrepareChild makes the child lead its own process group, so signals reach the whole tree.
func PrepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Contain is a no-op on Unix: the process group plus the client's exit handler cover the tree.
func Contain(int) (func(), error) { return func() {}, nil }

func Detach(cmd *exec.Cmd, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func ExeName(n string) string { return n }
