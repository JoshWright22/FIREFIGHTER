//go:build windows

package sysinfo

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ntdll    = windows.NewLazySystemDLL("ntdll.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	pdh      = windows.NewLazySystemDLL("pdh.dll")

	getLastInputInfo     = user32.NewProc("GetLastInputInfo")
	getForegroundWindow  = user32.NewProc("GetForegroundWindow")
	getWindowRect        = user32.NewProc("GetWindowRect")
	getClassName         = user32.NewProc("GetClassNameW")
	monitorFromWindow    = user32.NewProc("MonitorFromWindow")
	getMonitorInfo       = user32.NewProc("GetMonitorInfoW")
	getWindowThreadPid   = user32.NewProc("GetWindowThreadProcessId")
	getTickCount         = kernel32.NewProc("GetTickCount")
	setProcessInfo       = kernel32.NewProc("SetProcessInformation")
	ntSuspendProcess     = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess      = ntdll.NewProc("NtResumeProcess")
	ntSetInfoProcess     = ntdll.NewProc("NtSetInformationProcess")
	d3dkmtSetSchedPrio   = gdi32.NewProc("D3DKMTSetProcessSchedulingPriorityClass")
	pdhOpenQuery         = pdh.NewProc("PdhOpenQueryW")
	pdhAddEnglishCounter = pdh.NewProc("PdhAddEnglishCounterW")
	pdhCollect           = pdh.NewProc("PdhCollectQueryData")
	pdhGetArray          = pdh.NewProc("PdhGetFormattedCounterArrayW")
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func idleSeconds() float64 {
	info := struct{ size, time uint32 }{size: 8}
	if r, _, _ := getLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return -1
	}
	now, _, _ := getTickCount.Call()
	return float64(uint32(now)-info.time) / 1000
}

type rect struct{ l, t, r, b int32 }

// foregroundFullscreen returns the process name of a foreground window covering its whole monitor
// (games, fullscreen video), skipping the desktop itself.
func foregroundFullscreen() string {
	hwnd, _, _ := getForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var cls [64]uint16
	getClassName.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), 64)
	switch windows.UTF16ToString(cls[:]) {
	case "Progman", "WorkerW", "Shell_TrayWnd":
		return ""
	}
	var w rect
	if r, _, _ := getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&w))); r == 0 {
		return ""
	}
	mon, _, _ := monitorFromWindow.Call(hwnd, 2) // MONITOR_DEFAULTTONEAREST
	mi := struct {
		size          uint32
		monitor, work rect
		flags         uint32
	}{size: 40}
	if r, _, _ := getMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return ""
	}
	if w.l > mi.monitor.l || w.t > mi.monitor.t || w.r < mi.monitor.r || w.b < mi.monitor.b {
		return ""
	}
	var pid uint32
	getWindowThreadPid.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	n := Name(int(pid))
	if n == "explorer.exe" {
		return ""
	}
	return n
}

// platformReader holds a PDH query on the GPU Engine and GPU Process Memory counters: the only
// system-wide per-process GPU numbers under WDDM (nvidia-smi reports N/A there).
type platformReader struct {
	query      uintptr
	util, vram uintptr
	ok         bool
}

func newPlatformReader() *platformReader {
	p := &platformReader{}
	if pdhOpenQuery.Find() != nil {
		return p
	}
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&p.query))); r != 0 {
		return p
	}
	add := func(path string, h *uintptr) bool {
		s, _ := windows.UTF16PtrFromString(path)
		r, _, _ := pdhAddEnglishCounter.Call(p.query, uintptr(unsafe.Pointer(s)), 0, uintptr(unsafe.Pointer(h)))
		return r == 0
	}
	p.ok = add(`\GPU Engine(*)\Utilization Percentage`, &p.util) && add(`\GPU Process Memory(*)\Dedicated Usage`, &p.vram)
	pdhCollect.Call(p.query)
	return p
}

type pdhItem struct {
	name   *uint16
	status uint32
	_      uint32
	value  float64
}

func (p *platformReader) array(counter uintptr) map[string]float64 {
	out := map[string]float64{}
	var size, count uint32
	pdhGetArray.Call(counter, 0x200, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0) // PDH_FMT_DOUBLE
	if size == 0 {
		return out
	}
	buf := make([]byte, size)
	if r, _, _ := pdhGetArray.Call(counter, 0x200, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return out
	}
	items := unsafe.Slice((*pdhItem)(unsafe.Pointer(&buf[0])), count)
	for _, it := range items {
		if it.status <= 1 { // PDH_CSTATUS_VALID_DATA / NEW_DATA
			out[windows.UTF16PtrToString(it.name)] += it.value
		}
	}
	return out
}

// procGPU returns per-pid GPU engine use (summed over engines, capped at 100) and dedicated VRAM.
func (p *platformReader) procGPU() map[int]ProcGPU {
	m := map[int]ProcGPU{}
	if !p.ok {
		return m
	}
	if r, _, _ := pdhCollect.Call(p.query); r != 0 {
		return m
	}
	for inst, v := range p.array(p.util) {
		if pid := pidOf(inst); pid > 0 {
			g := m[pid]
			g.Util = min(100, g.Util+v)
			if strings.HasSuffix(inst, "engtype_3D") {
				g.Util3D = min(100, g.Util3D+v)
			}
			m[pid] = g
		}
	}
	for inst, v := range p.array(p.vram) {
		if pid := pidOf(inst); pid > 0 {
			g := m[pid]
			g.VRAMGB += v / (1 << 30)
			m[pid] = g
		}
	}
	return m
}

func pidOf(instance string) int {
	f := strings.Split(instance, "_")
	if len(f) < 2 || f[0] != "pid" {
		return 0
	}
	pid, _ := strconv.Atoi(f[1])
	return pid
}

// ---- controls ---------------------------------------------------------------------------------

func open(pid int, access uint32) (windows.Handle, error) {
	return windows.OpenProcess(access, false, uint32(pid))
}

// Background gives a process idle CPU priority, idle GPU scheduling priority, very low memory
// priority (Windows trims its pages before the user's) and very low I/O priority.
func Background(pid int) error {
	h, err := open(pid, windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_INFORMATION)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.SetPriorityClass(h, windows.IDLE_PRIORITY_CLASS); err != nil {
		return err
	}
	if d3dkmtSetSchedPrio.Find() == nil {
		d3dkmtSetSchedPrio.Call(uintptr(h), 0) // D3DKMT_SCHEDULINGPRIORITYCLASS_IDLE
	}
	memPrio := uint32(1) // MEMORY_PRIORITY_VERY_LOW
	setProcessInfo.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&memPrio)), 4)
	io := uint32(0) // IoPriorityVeryLow
	ntSetInfoProcess.Call(uintptr(h), 33, uintptr(unsafe.Pointer(&io)), 4)
	return nil
}

// SetCPUs pins a process to the given logical CPUs (first 64; one processor group).
func SetCPUs(pid int, cpus []int) error {
	var mask uintptr
	for _, c := range cpus {
		if c < 64 {
			mask |= 1 << uint(c)
		}
	}
	if mask == 0 {
		return nil
	}
	h, err := open(pid, windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_INFORMATION)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	r, _, e := kernel32.NewProc("SetProcessAffinityMask").Call(uintptr(h), mask)
	if r == 0 {
		return e
	}
	return nil
}

func Suspend(pid int) error { return ntCall(ntSuspendProcess, pid) }
func Resume(pid int) error  { return ntCall(ntResumeProcess, pid) }

func ntCall(p *windows.LazyProc, pid int) error {
	h, err := open(pid, 0x0800) // PROCESS_SUSPEND_RESUME
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if r, _, _ := p.Call(uintptr(h)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func Kill(pid int) error {
	h, err := open(pid, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 137)
}

// Terminate asks politely; Windows has no SIGTERM for arbitrary processes, so jobs get the
// checkpoint flag file instead and this kills.
func Terminate(pid int) error { return Kill(pid) }

// PrepareChild puts the child in its own process group (so the guard can address the tree).
func PrepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// Contain puts a started child in a Job Object that kills the whole tree when the client exits, so
// no orphaned workers survive a crashed or killed `firefighter run`.
func Contain(pid int) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return func() {}, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return func() {}, err
	}
	h, err := open(pid, windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE)
	if err != nil {
		windows.CloseHandle(job)
		return func() {}, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return func() {}, err
	}
	return func() { windows.CloseHandle(job) }, nil
}

// Detach starts a process with no console and no tie to the caller (for the daemon and detached
// jobs). breakaway also leaves the caller's Job Object, so closing an agent's shell doesn't kill it;
// callers retry without it when the job forbids breakaway.
func Detach(cmd *exec.Cmd, breakaway bool) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | 0x00000008) // DETACHED_PROCESS
	if breakaway {
		flags |= 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}

func ExeName(n string) string {
	if filepath.Ext(n) == "" {
		return n + ".exe"
	}
	return n
}
