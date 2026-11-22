//go:build windows

// Command firefighterw starts the firefighter guard at logon without a window. It is built as a
// Windows GUI program (-H windowsgui), so it has no console of its own, and the per-user Run key
// names it directly: no conhost --headless, which hides a window the way malware does (MITRE
// T1564.003) and is what Defender's Persistence/Bearfoos models flagged (security audit
// 2026-11-22). It starts firefighter.exe from its own folder by full path and exits.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const createNoWindow = 0x08000000

func main() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	exe := filepath.Join(filepath.Dir(self), "firefighter.exe")
	if _, err := os.Stat(exe); err != nil {
		os.Exit(1)
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	_ = cmd.Process.Release()
}
