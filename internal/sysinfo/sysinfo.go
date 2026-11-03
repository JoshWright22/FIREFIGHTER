// Package sysinfo reads the machine (CPU, RAM, GPU, the human's input and foreground) and applies
// controls to job processes, on Windows, macOS and Linux. Everything not launched by firefighter,
// including Docker containers, Kubernetes pods, games and the user's own apps, shows up here as
// non-job load and is protected by the reserve.
package sysinfo

import (
	"context"
	"encoding/csv"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

type GPU struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Util     float64 `json:"util"`
	VRAMGB   float64 `json:"vram_gb"`
	UsedGB   float64 `json:"used_gb"`
	Vendor   string  `json:"vendor"`
	PerProc  bool    `json:"-"`
}

type ProcGPU struct {
	Util   float64 // percent of the GPU this process's engines used
	VRAMGB float64 // dedicated VRAM held
}

type Snapshot struct {
	Time       time.Time
	CPUs       int
	CPUBusy    float64 // logical CPUs busy, machine-wide
	RAMTotalGB float64
	RAMAvailGB float64
	GPUs       []GPU
	ProcGPU    map[int]ProcGPU
	Idle       float64 // seconds since the human's last input; -1 unknown
	Foreground string  // process name of a fullscreen foreground window, "" if none
}

func (s Snapshot) VRAMTotal() (total, used float64) {
	for _, g := range s.GPUs {
		total += g.VRAMGB
		used += g.UsedGB
	}
	return
}

func (s Snapshot) GPUUtil() float64 {
	m := 0.0
	for _, g := range s.GPUs {
		if g.Util > m {
			m = g.Util
		}
	}
	return m
}

// Reader keeps state between samples (CPU deltas, Windows PDH queries).
type Reader struct {
	gpu      []GPU
	gpuAt    time.Time
	procGPU  map[int]ProcGPU
	platform *platformReader
}

func NewReader() *Reader { return &Reader{platform: newPlatformReader()} }

// Read samples the machine. GPU vendor tools are slow, so they refresh at most every 2 s.
func (r *Reader) Read() Snapshot {
	s := Snapshot{Time: time.Now(), CPUs: runtime.NumCPU(), Idle: -1}
	if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
		s.CPUBusy = p[0] / 100 * float64(s.CPUs)
	}
	if v, err := mem.VirtualMemory(); err == nil {
		s.RAMTotalGB = gb(v.Total)
		s.RAMAvailGB = gb(v.Available)
	}
	if time.Since(r.gpuAt) > 2*time.Second {
		r.gpu = readGPUs()
		r.gpuAt = time.Now()
	}
	s.GPUs = r.gpu
	s.ProcGPU = r.platform.procGPU()
	if len(s.ProcGPU) == 0 {
		s.ProcGPU = nvidiaProcs()
	}
	s.Idle = idleSeconds()
	s.Foreground = foregroundFullscreen()
	return s
}

func gb(b uint64) float64 { return float64(b) / (1 << 30) }

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}

func readGPUs() []GPU {
	if g := nvidiaGPUs(); len(g) > 0 {
		return g
	}
	return amdGPUs()
}

func nvidiaGPUs() []GPU {
	out, err := run(5*time.Second, "nvidia-smi", "--query-gpu=index,name,utilization.gpu,memory.total,memory.used", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	rows, _ := csv.NewReader(strings.NewReader(out)).ReadAll()
	var gs []GPU
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		g := GPU{Vendor: "nvidia", Name: strings.TrimSpace(r[1])}
		g.Index, _ = strconv.Atoi(strings.TrimSpace(r[0]))
		g.Util = num(r[2])
		g.VRAMGB = num(r[3]) / 1024
		g.UsedGB = num(r[4]) / 1024
		gs = append(gs, g)
	}
	return gs
}

// nvidiaProcs gives per-process VRAM where the driver reports it (Linux; Windows WDDM reports N/A,
// so Windows uses its own GPU performance counters instead).
func nvidiaProcs() map[int]ProcGPU {
	out, err := run(5*time.Second, "nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	m := map[int]ProcGPU{}
	if err != nil {
		return m
	}
	rows, _ := csv.NewReader(strings.NewReader(out)).ReadAll()
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(r[0]))
		if err != nil {
			continue
		}
		p := m[pid]
		p.VRAMGB += num(r[1]) / 1024
		m[pid] = p
	}
	return m
}

// amdGPUs reads ROCm's rocm-smi (Linux AMD).
func amdGPUs() []GPU {
	out, err := run(5*time.Second, "rocm-smi", "--showuse", "--showmeminfo", "vram", "--csv")
	if err != nil {
		return nil
	}
	rows, _ := csv.NewReader(strings.NewReader(out)).ReadAll()
	if len(rows) < 2 {
		return nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(h)] = i
	}
	var gs []GPU
	for i, r := range rows[1:] {
		g := GPU{Index: i, Vendor: "amd", Name: "AMD GPU"}
		for k, c := range col {
			if c >= len(r) {
				continue
			}
			switch {
			case strings.Contains(k, "gpu use"):
				g.Util = num(r[c])
			case strings.Contains(k, "total memory"):
				g.VRAMGB = num(r[c]) / (1 << 30)
			case strings.Contains(k, "total used memory"):
				g.UsedGB = num(r[c]) / (1 << 30)
			}
		}
		gs = append(gs, g)
	}
	return gs
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// Tree returns pid and every descendant.
func Tree(pid int) []int {
	out := []int{pid}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return out
	}
	var walk func(*process.Process, int)
	walk = func(p *process.Process, depth int) {
		if depth > 12 {
			return
		}
		kids, _ := p.Children()
		for _, k := range kids {
			out = append(out, int(k.Pid))
			walk(k, depth+1)
		}
	}
	walk(p, 0)
	return out
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	ok, _ := process.PidExists(int32(pid))
	return ok
}

// Usage of a set of pids: CPU seconds so far and resident RAM.
func Usage(pids []int) (cpuSeconds, ramGB float64) {
	for _, pid := range pids {
		p, err := process.NewProcess(int32(pid))
		if err != nil {
			continue
		}
		if t, err := p.Times(); err == nil {
			cpuSeconds += t.User + t.System
		}
		if m, err := p.MemoryInfo(); err == nil {
			ramGB += gb(m.RSS)
		}
	}
	return
}

// Names returns lowercase process names currently running (for exclusive apps).
func Names() map[string]bool {
	m := map[string]bool{}
	ps, _ := process.Processes()
	for _, p := range ps {
		if n, err := p.Name(); err == nil {
			m[strings.ToLower(n)] = true
		}
	}
	return m
}

func Name(pid int) string {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return ""
	}
	n, _ := p.Name()
	return strings.ToLower(n)
}

// Containers lists running Docker containers with their CPU and memory, so the status can say where
// non-job load comes from. Empty when Docker isn't installed or running.
type Container struct {
	Name  string  `json:"name"`
	CPU   float64 `json:"cpu_pct"`
	MemGB float64 `json:"mem_gb"`
}

func Containers() []Container {
	out, err := run(8*time.Second, "docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}")
	if err != nil {
		return nil
	}
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		c := Container{Name: f[0], CPU: num(strings.TrimSuffix(f[1], "%"))}
		c.MemGB = size(strings.TrimSpace(strings.Split(f[2], "/")[0]))
		cs = append(cs, c)
	}
	return cs
}

func size(s string) float64 {
	units := []struct {
		suffix string
		mult   float64
	}{{"GiB", 1}, {"MiB", 1.0 / 1024}, {"KiB", 1.0 / (1 << 20)}, {"GB", 1e9 / (1 << 30)}, {"MB", 1e6 / (1 << 30)}, {"kB", 1e3 / (1 << 30)}, {"B", 1.0 / (1 << 30)}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			return num(strings.TrimSuffix(s, u.suffix)) * u.mult
		}
	}
	return num(s)
}
