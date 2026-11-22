//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

var stopSignals = func() []os.Signal {
	s := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) { // under nohup a hangup must stay ignored
		s = append(s, syscall.SIGHUP)
	}
	return s
}()

// meantToOutlive: started under nohup, so the run is meant to outlive the shell that launched it.
func meantToOutlive() bool { return signal.Ignored(syscall.SIGHUP) }
