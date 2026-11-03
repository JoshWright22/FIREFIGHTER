package sysinfo

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows GPUs from any vendor: DXGI lists the adapters (vendor, dedicated VRAM, LUID); the GPU
// Adapter Memory and GPU Engine performance counters, keyed by the same LUID, give each one's VRAM
// in use and utilization. nvidia-smi stays the source for NVIDIA cards; this adds AMD and Intel
// cards (no vendor tool needed) and covers NVIDIA when nvidia-smi is missing.

var (
	dxgi               = windows.NewLazySystemDLL("dxgi.dll")
	createDXGIFactory1 = dxgi.NewProc("CreateDXGIFactory1")
	iidFactory1        = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
)

type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	LuidLow               uint32
	LuidHigh              int32
	Flags                 uint32
}

type dxgiAdapter struct {
	name   string
	vendor string
	vram   float64
	luid   string // as the performance counters write it: luid_0x00000000_0x0000D1B2
}

// vtbl returns method i of a COM object's virtual table.
func vtbl(obj unsafe.Pointer, i int) uintptr {
	table := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(table, i*int(unsafe.Sizeof(uintptr(0)))))
}

// dxgiAdapters lists hardware adapters with real dedicated VRAM (no software renderer, no iGPU
// with a token carve-out).
func dxgiAdapters() []dxgiAdapter {
	if createDXGIFactory1.Find() != nil {
		return nil
	}
	var factory unsafe.Pointer
	if r, _, _ := createDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidFactory1)), uintptr(unsafe.Pointer(&factory))); r != 0 || factory == nil {
		return nil
	}
	defer syscall.SyscallN(vtbl(factory, 2), uintptr(factory)) // Release
	var out []dxgiAdapter
	for i := 0; i < 16; i++ {
		var adapter unsafe.Pointer
		if r, _, _ := syscall.SyscallN(vtbl(factory, 12), uintptr(factory), uintptr(i), uintptr(unsafe.Pointer(&adapter))); r != 0 || adapter == nil {
			break // DXGI_ERROR_NOT_FOUND: no more adapters
		}
		var d dxgiAdapterDesc1
		r, _, _ := syscall.SyscallN(vtbl(adapter, 10), uintptr(adapter), uintptr(unsafe.Pointer(&d))) // GetDesc1
		syscall.SyscallN(vtbl(adapter, 2), uintptr(adapter))
		if r != 0 || d.Flags&2 != 0 || d.VendorID == 0x1414 { // software adapter / Microsoft Basic Render
			continue
		}
		gb := float64(d.DedicatedVideoMemory) / (1 << 30)
		if gb < 1 {
			continue // integrated graphics: shared system RAM, admitted as RAM
		}
		vendor := map[uint32]string{0x10de: "nvidia", 0x1002: "amd", 0x1022: "amd", 0x8086: "intel"}[d.VendorID]
		if vendor == "" {
			vendor = fmt.Sprintf("0x%04x", d.VendorID)
		}
		out = append(out, dxgiAdapter{
			name: windows.UTF16ToString(d.Description[:]), vendor: vendor, vram: gb,
			luid: strings.ToLower(fmt.Sprintf("luid_0x%08x_0x%08x", uint32(d.LuidHigh), d.LuidLow)),
		})
	}
	return out
}
