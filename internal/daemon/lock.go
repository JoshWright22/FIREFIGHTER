package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JoshWright22/firefighter/internal/config"
	"github.com/JoshWright22/firefighter/internal/sysinfo"
)

// Lock makes sure only one guard runs per user. A lock left by a dead daemon is taken over.
func Lock() (func(), error) {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(config.Home(), "daemon.lock")
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		b, _ := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && pid != os.Getpid() && sysinfo.Alive(pid) && strings.Contains(sysinfo.Name(pid), "firefighter") {
			return nil, fmt.Errorf("the guard is already running (pid %d)", pid)
		}
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("couldn't take %s", path)
}
