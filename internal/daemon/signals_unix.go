//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
