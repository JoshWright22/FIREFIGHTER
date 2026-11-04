package sysinfo

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32    = windows.NewLazySystemDLL("advapi32.dll")
	accessCheck = advapi32.NewProc("AccessCheck")
)

// canChange says whether this user (its real, possibly filtered token) holds any of the rights in
// mask on the object: then an agent could replace or redirect it, whoever owns it.
const (
	changeFile   = 0x2 | 0x4 | 0x10000 | 0x40000 | 0x80000        // write, append, DELETE, WRITE_DAC, WRITE_OWNER
	changeOwnDir = 0x2 | 0x4 | 0x40 | 0x10000 | 0x40000 | 0x80000 // + add files/folders (DLL planting), delete children
	changeUpDir  = 0x40 | 0x10000 | 0x40000 | 0x80000             // delete/rename children, DELETE, re-permission (adding new folders is harmless)
)

func canChange(sd *windows.SECURITY_DESCRIPTOR, mask uint32) bool {
	var proc windows.Token
	if windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &proc) != nil {
		return true
	}
	defer proc.Close()
	var imp windows.Token
	if windows.DuplicateTokenEx(proc, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &imp) != nil {
		return true
	}
	defer imp.Close()
	mapping := [4]uint32{0x120089, 0x120116, 0x1200A0, 0x1F01FF} // FILE_GENERIC_READ/WRITE/EXECUTE, FILE_ALL_ACCESS
	var privs [256]byte
	privLen := uint32(len(privs))
	var granted, status uint32
	r, _, _ := accessCheck.Call(uintptr(unsafe.Pointer(sd)), uintptr(imp), 0x02000000, // MAXIMUM_ALLOWED
		uintptr(unsafe.Pointer(&mapping[0])), uintptr(unsafe.Pointer(&privs[0])), uintptr(unsafe.Pointer(&privLen)),
		uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return true // can't tell: don't trust it
	}
	return status != 0 && granted&mask != 0
}

// adminOwned: path and every folder above it are owned by TrustedInstaller, SYSTEM or the
// Administrators group (what a user or agent creates is owned by that user, and a standard user
// can't hand ownership to the system), and this user can't write, delete or re-permission any of
// them (an admin-owned file in a folder users can write to isn't safe).
func adminOwned(path string) bool {
	trusted := map[string]bool{
		"S-1-5-18":     true, // SYSTEM
		"S-1-5-32-544": true, // Administrators
		"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true, // TrustedInstaller
	}
	for depth, p := 0, filepath.Clean(path); ; depth, p = depth+1, filepath.Dir(p) {
		mask := uint32(changeUpDir)
		switch depth {
		case 0:
			mask = changeFile
		case 1:
			mask = changeOwnDir
		}
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
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
		if err != nil || owner == nil || !trusted[owner.String()] || canChange(sd, mask) {
			return false // not the system's, or this user could replace it anyway
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
