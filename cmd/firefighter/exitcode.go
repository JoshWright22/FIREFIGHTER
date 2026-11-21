package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

// exitCodeOf is a finished job's exit code as shells and the failure rules know it: a Unix signal
// death is 128+signal (137 for the OOM killer's SIGKILL; Go reports -1), and a Windows NTSTATUS
// crash code is its signed value (-1073741819 for an access violation; Go reports it unsigned).
func exitCodeOf(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	code := ee.ExitCode()
	if runtime.GOOS == "windows" {
		return int(int32(uint32(code)))
	}
	return code
}
