package setup

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const installerManagesAutostart = true

func removeStartupShortcut() error {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, "firefighter.lnk"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
