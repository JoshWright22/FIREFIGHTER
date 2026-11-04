package client

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/daemon"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// LogPath is where a detached job's output goes.
func LogPath(id string) string { return filepath.Join(config.Home(), "logs", id+".log") }

// Detached starts `firefighter run --id ID <runArgs...>` in the background with its output in a log
// file, and returns the job id at once. `firefighter wait ID` (or the firefighter_wait MCP tool)
// blocks until it ends and returns the receipt.
func Detached(runArgs []string, cwd string) (id, log string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	id = daemon.NewID()
	log = LogPath(id)
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		return "", "", err
	}
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	args := append([]string{"run", "--id", id, "--log", log}, runArgs...)
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, f, f
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		sysinfo.Detach(cmd, breakaway)
		if err = cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return id, log, nil
		}
	}
	return "", "", err
}

// Tail returns the last n bytes of a file.
func Tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	_, _ = f.ReadAt(buf, off)
	return string(buf)
}
