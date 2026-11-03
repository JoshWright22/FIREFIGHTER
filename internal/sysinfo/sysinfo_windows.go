//go:build windows

package sysinfo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	isZoomed             = user32.NewProc("IsZoomed")
	getWindowLong        = user32.NewProc("GetWindowLongW")
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
	// A maximized window with a title bar isn't fullscreen (its rect overshoots the monitor a bit).
	if z, _, _ := isZoomed.Call(hwnd); z != 0 {
		return ""
	}
	gwlStyle := ^uintptr(15)                                                         // GWL_STYLE = -16
	if st, _, _ := getWindowLong.Call(hwnd, gwlStyle); st&0x00C00000 == 0x00C00000 { // WS_CAPTION
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

// foregroundApp names the process that owns the focused window.
func foregroundApp() string {
	hwnd, _, _ := getForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	getWindowThreadPid.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return Name(int(pid))
}

// platformReader holds a PDH query on the GPU Engine and GPU Process Memory counters: the only
// system-wide per-process GPU numbers under WDDM (nvidia-smi reports N/A there).
type platformReader struct {
	query      uintptr
	util, vram uintptr
	adapterMem uintptr
	ok         bool
	adapters   []dxgiAdapter
	adaptersAt time.Time
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
	add(`\GPU Adapter Memory(*)\Dedicated Usage`, &p.adapterMem)
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

// gpus returns discrete adapters of vendors no vendor tool already listed (have), with VRAM in use
// and utilization from the counters (sampled at the last procGPU call, under a second ago).
func (p *platformReader) gpus(have map[string]bool) []GPU {
	if p.adapters == nil || time.Since(p.adaptersAt) > time.Minute {
		p.adapters, p.adaptersAt = dxgiAdapters(), time.Now()
	}
	if len(p.adapters) == 0 {
		return nil
	}
	used, util := map[string]float64{}, map[string]map[string]float64{}
	if p.ok {
		for inst, v := range p.array(p.adapterMem) {
			if l := luidOf(inst); l != "" {
				used[l] += v / (1 << 30)
			}
		}
		for inst, v := range p.array(p.util) {
			l, eng := luidOf(inst), ""
			if k := strings.LastIndex(inst, "engtype_"); k >= 0 {
				eng = inst[k+8:]
			}
			if l == "" {
				continue
			}
			if util[l] == nil {
				util[l] = map[string]float64{}
			}
			util[l][eng] += v
		}
	}
	var out []GPU
	idx := map[string]int{}
	for _, a := range p.adapters {
		i := idx[a.vendor]
		idx[a.vendor]++
		if have[a.vendor] {
			continue
		}
		busy := 0.0
		for _, v := range util[a.luid] { // the busiest engine type is the adapter's utilization
			busy = max(busy, min(100, v))
		}
		out = append(out, GPU{Index: i, Name: a.name, Vendor: a.vendor, VRAMGB: a.vram, UsedGB: used[a.luid], Util: busy})
	}
	return out
}

// luidOf pulls "luid_0x..._0x..." out of a GPU counter instance name.
func luidOf(inst string) string {
	inst = strings.ToLower(inst)
	i := strings.Index(inst, "luid_0x")
	if i < 0 || len(inst) < i+26 {
		return ""
	}
	return inst[i : i+26]
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
	if logicalCPUs() > 64 {
		return setCPUSets(pid, cpus) // an affinity mask reaches one processor group of 64
	}
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

func guard(pid int) error {
	if pid <= 4 || pid == os.Getpid() {
		return fmt.Errorf("refusing to touch pid %d", pid)
	}
	return nil
}

func Suspend(pid int) error { return ntCall(ntSuspendProcess, pid) }
func Resume(pid int) error  { return ntCall(ntResumeProcess, pid) }

func ntCall(p *windows.LazyProc, pid int) error {
	if err := guard(pid); err != nil {
		return err
	}
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
	if err := guard(pid); err != nil {
		return err
	}
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

var getConsoleWindow = kernel32.NewProc("GetConsoleWindow")

// PrepareChild puts the child in its own process group. With no console of our own (a detached
// run, the MCP server) the child gets none either, so nothing flashes over the user's screen. A
// `cmd /c <line>` goes to cmd.exe verbatim: Go's C-runtime quoting breaks cmd's own parsing.
func PrepareChild(cmd *exec.Cmd, argv []string) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if w, _, _ := getConsoleWindow.Call(); w == 0 {
		flags |= 0x08000000 // CREATE_NO_WINDOW
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if len(argv) >= 3 && strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") == "cmd" && strings.EqualFold(argv[1], "/c") {
		// One argument is passed as typed. Several (their quotes already stripped by the caller's
		// shell): re-quote the ones with spaces, so a path like C:\My Files survives.
		rest := argv[2:]
		if len(rest) > 1 {
			q := make([]string, len(rest))
			for i, a := range rest {
				if strings.ContainsAny(a, " \t") && !strings.HasPrefix(a, `"`) {
					a = `"` + a + `"`
				}
				q[i] = a
			}
			rest = q
		}
		cmd.SysProcAttr.CmdLine = `cmd /s /c "` + strings.Join(rest, " ") + `"`
	}
}

func RestoreForeground() {}

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

// PrepareBackground starts a helper (docker, the harvester) with no window; Windows has no terminal
// foreground to lose.
func PrepareBackground(cmd *exec.Cmd) { PrepareChild(cmd, nil) }

var (
	getSystemCpuSetInformation = kernel32.NewProc("GetSystemCpuSetInformation")
	setProcessDefaultCpuSets   = kernel32.NewProc("SetProcessDefaultCpuSets")
)

// setCPUSets restricts a process to CPUs across all processor groups (Windows 10+ CPU Sets), for
// machines with more than 64 logical CPUs. CPU i is group*64 + its index in the group.
func setCPUSets(pid int, cpus []int) error {
	if getSystemCpuSetInformation.Find() != nil || setProcessDefaultCpuSets.Find() != nil {
		return nil
	}
	var size uint32
	getSystemCpuSetInformation.Call(0, 0, uintptr(unsafe.Pointer(&size)), 0, 0)
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	if r, _, e := getSystemCpuSetInformation.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&size)), 0, 0); r == 0 {
		return e
	}
	want := map[int]bool{}
	for _, c := range cpus {
		want[c] = true
	}
	var ids []uint32
	for off := uint32(0); off+16 <= size; {
		recSize := *(*uint32)(unsafe.Pointer(&buf[off]))
		if recSize == 0 {
			break
		}
		if *(*uint32)(unsafe.Pointer(&buf[off+4])) == 0 { // CpuSetInformation
			id := *(*uint32)(unsafe.Pointer(&buf[off+8]))
			group := *(*uint16)(unsafe.Pointer(&buf[off+12]))
			logical := buf[off+14]
			if want[int(group)*64+int(logical)] {
				ids = append(ids, id)
			}
		}
		off += recSize
	}
	if len(ids) == 0 {
		return nil
	}
	h, err := open(pid, 0x2000) // PROCESS_SET_LIMITED_INFORMATION
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if r, _, e := setProcessDefaultCpuSets.Call(uintptr(h), uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids))); r == 0 {
		return e
	}
	return nil
}
