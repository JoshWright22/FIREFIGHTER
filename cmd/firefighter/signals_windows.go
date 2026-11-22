//go:build windows

package main

import "os"

var stopSignals = []os.Signal{os.Interrupt}

// meantToOutlive: Windows has no nohup; work meant to outlive its launcher uses --detach.
func meantToOutlive() bool { return false }
