//go:build darwin

package sysinfo

import (
	"regexp"
	"strconv"
	"syscall"
	"time"
)

type platformReader struct{}

func newPlatformReader() *platformReader         { return &platformReader{} }
func (*platformReader) procGPU() map[int]ProcGPU { return nil }
func foregroundFullscreen() string               { return "" }
func foregroundApp() string                      { return "" }

var hidIdle = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// idleSeconds reads IOHIDSystem's HIDIdleTime (nanoseconds).
func idleSeconds() float64 {
	out, err := run(2*time.Second, "ioreg", "-c", "IOHIDSystem", "-d", "4")
	if err != nil {
		return -1
	}
	m := hidIdle.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	ns, _ := strconv.ParseFloat(m[1], 64)
	return ns / 1e9
}

// Background: nice 19 and the background QoS band (throttled CPU, I/O and timers on Apple silicon).
func Background(pid int) error {
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19); err != nil {
		return err
	}
	// taskpolicy is a process spawn; callers hold the daemon lock, so it runs in the background.
	go run(2*time.Second, "taskpolicy", "-b", "-p", strconv.Itoa(pid))
	return nil
}

// SetCPUs is a no-op: macOS has no CPU affinity; the background QoS band keeps jobs off the
// performance cores when the user needs them.
func SetCPUs(int, []int) error { return nil }

// gpus: GPUs beyond what vendor tools list. Linux: nvidia-smi, rocm-smi and sysfs cover it; macOS
// GPUs share system memory (admitted as RAM).
func (*platformReader) gpus(map[string]bool) []GPU { return nil }
