package config

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// osDefaultHomes is the real state folder as Windows reports it, whatever LOCALAPPDATA says.
func osDefaultHomes() []string {
	d, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || d == "" {
		return nil
	}
	return []string{filepath.Join(d, "firefighter")}
}
