package sysinfo

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// adminOwned: path and every folder above it are owned by TrustedInstaller, SYSTEM or the
// Administrators group (what a user or agent creates is owned by that user, and a standard user
// can't hand ownership to the system).
func adminOwned(path string) bool {
	trusted := map[string]bool{
		"S-1-5-18":     true, // SYSTEM
		"S-1-5-32-544": true, // Administrators
		"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true, // TrustedInstaller
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			// Users can't even read who owns the WindowsApps root (Store apps like Windows
			// Terminal live under it), nor create anything in it. Only that exact folder is
			// excused; anywhere else, unreadable means untrusted.
			if strings.EqualFold(p, filepath.Join(os.Getenv("ProgramFiles"), "WindowsApps")) {
				continue
			}
			return false
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !trusted[owner.String()] {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
