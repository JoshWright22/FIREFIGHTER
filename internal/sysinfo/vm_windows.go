//go:build windows

package sysinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CanTrim: a suspended process's memory can be moved out of RAM here (see Trim).
const CanTrim = true

var setProcessWorkingSetSizeEx = kernel32.NewProc("SetProcessWorkingSetSizeEx")

// Trim empties a process's working set: its pages leave RAM for the pagefile and come back when it
// touches them again. On a suspended process that frees its RAM for as long as it stays suspended,
// without losing anything it has computed. VRAM is not touched.
func Trim(pid int) error {
	if err := guard(pid); err != nil {
		return err
	}
	h, err := open(pid, windows.PROCESS_SET_QUOTA|windows.PROCESS_QUERY_INFORMATION)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if r, _, e := setProcessWorkingSetSizeEx.Call(uintptr(h), ^uintptr(0), ^uintptr(0), 0); r == 0 {
		return e
	}
	return nil
}

// vmHost sums the working sets of the WSL2 / Hyper-V VM worker processes (vmmem, vmmemWSL). They
// are protected processes: only the system-wide process list shows their memory.
func vmHost() float64 {
	size := uint32(1 << 20)
	for try := 0; try < 4; try++ {
		buf := make([]byte, size)
		var need uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &need)
		if err != nil {
			if need <= size {
				return 0
			}
			size = need + 1<<16 // the list grew meanwhile
			continue
		}
		var total uint64
		for off := uint32(0); int(off)+int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})) <= len(buf); {
			p := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
			if strings.HasPrefix(strings.ToLower(p.ImageName.String()), "vmmem") {
				total += uint64(p.WorkingSetSize)
			}
			if p.NextEntryOffset == 0 {
				break
			}
			off += p.NextEntryOffset
		}
		return gb(total)
	}
	return 0
}

// wsl runs the system's own wsl.exe (never one found on the PATH).
func wsl(timeout time.Duration, args ...string) (string, error) {
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "wsl.exe")
	if _, err := os.Stat(exe); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	hideWindow(cmd)
	out, err := cmd.Output()
	// Older WSL builds ignore WSL_UTF8 and print UTF-16: the names are ASCII either way.
	return strings.ReplaceAll(string(out), "\x00", ""), err
}

// vmDistro names a distro that is already running (asking a stopped one would start it). Every
// WSL2 distro shares one VM and one kernel, so any of them reads and reclaims the same memory;
// Docker Desktop's is preferred because it stays up as long as Docker does.
func vmDistro() string {
	out, err := wsl(5*time.Second, "--list", "--running", "--quiet")
	if err != nil {
		return ""
	}
	first := ""
	for _, n := range strings.Fields(out) {
		if n == "docker-desktop" {
			return n
		}
		if first == "" {
			first = n
		}
	}
	return first
}

func vmGuest() (name, meminfo string, ok bool) {
	if name = vmDistro(); name == "" {
		return "", "", false
	}
	out, err := wsl(5*time.Second, "-d", name, "-u", "root", "-e", "cat", "/proc/meminfo")
	return name, out, err == nil && strings.Contains(out, "MemTotal")
}

func vmReclaim(name string) error {
	if name == "" {
		name = vmDistro() // the guest's numbers weren't read: any running distro reaches the same VM
	}
	if name == "" {
		return errors.New("no running WSL distro")
	}
	// Page cache only (1): dropping dentries and inodes too would cost a running build more than
	// it returns. Compaction gathers the freed pages into blocks WSL hands back to Windows.
	_, err := wsl(90*time.Second, "-d", name, "-u", "root", "-e", "sh", "-c",
		"echo 1 > /proc/sys/vm/drop_caches && { echo 1 > /proc/sys/vm/compact_memory; } 2>/dev/null; true")
	return err
}

// volume is the drive a path is on ("C:\").
func volume(abs string) string {
	return strings.ToUpper(filepath.VolumeName(abs)) + `\`
}
