//go:build windows

package sysinfo

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// Windows 节点是真实支持的场景（本项目就在 Windows 上做过端到端联调），
// 不能像 sysinfo_other.go 那样返回 1 字节的占位值 —— 那会让面板的负载图
// 永远显示"内存 1B / 磁盘 1B"。这里直接调 kernel32，不引第三方依赖。

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	psapi                  = syscall.NewLazyDLL("psapi.dll")
	procGetPerformanceInfo = psapi.NewProc("GetPerformanceInfo")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// PERFORMANCE_INFORMATION（psapi!GetPerformanceInfo）。Cb 是 uint32，后面在 64 位下
// 有 4 字节填充，Go 结构体把 uintptr 对齐到 8 正好与 Win32 布局一致。
type performanceInformation struct {
	Cb                uint32
	CommitTotal       uintptr
	CommitLimit       uintptr
	CommitPeak        uintptr
	PhysicalTotal     uintptr
	PhysicalAvailable uintptr
	SystemCache       uintptr
	KernelTotal       uintptr
	KernelPaged       uintptr
	KernelNonpaged    uintptr
	PageSize          uintptr
	HandleCount       uint32
	ProcessCount      uint32
	ThreadCount       uint32
}

type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (f filetime) value() uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// cpu 采样需要两次读数求差；第一次调用返回 0（面板只要求 0..100 的数字）。
var (
	cpuMu     sync.Mutex
	lastIdle  uint64
	lastTotal uint64
)

func cpuPercent() float64 {
	var idle, kernel, user filetime
	if ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	); ret == 0 {
		return 0
	}
	// kernel 时间已包含 idle，所以总时间 = kernel + user。
	i, t := idle.value(), kernel.value()+user.value()

	cpuMu.Lock()
	defer cpuMu.Unlock()
	di, dt := i-lastIdle, t-lastTotal
	lastIdle, lastTotal = i, t
	if lastTotal == 0 || dt == 0 || di > dt {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

func rootPath() string {
	if exe, err := os.Executable(); err == nil {
		if vol := filepath.VolumeName(exe); vol != "" {
			return vol + `\`
		}
	}
	return `C:\`
}

func diskUsage() (total, used int64) {
	p, err := syscall.UTF16PtrFromString(rootPath())
	if err != nil {
		return 0, 0
	}
	var freeToCaller, totalBytes, totalFree uint64
	if ret, _, _ := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	); ret == 0 {
		return 0, 0
	}
	return int64(totalBytes), int64(totalBytes - totalFree)
}

// Stat 采集 CPU/内存/交换/磁盘。任何一项取不到就给 0（面板校验要求 ≥0 且存在）。
func Stat() (Load, error) {
	var l Load
	l.CPUPercent = cpuPercent()

	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if ret, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&ms))); ret != 0 {
		l.MemTotal = int64(ms.TotalPhys)
		l.MemUsed = int64(ms.TotalPhys - ms.AvailPhys)
	}

	// Windows 没有独立 swap 分区。用 psapi 的提交量推算页面文件占用：
	//   swapTotal = 提交上限 - 物理内存
	//   swapUsed  = 已提交量 - 已用物理内存
	// 比 "TotalPageFile - TotalPhys" 准，因为 GlobalMemoryStatusEx 的这两个字段
	// 在页面文件接近写满时会把可用量报成 0，算出来恒等于"用满"。
	var pi performanceInformation
	pi.Cb = uint32(unsafe.Sizeof(pi))
	if ret, _, _ := procGetPerformanceInfo.Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.Cb)); ret != 0 && pi.PageSize != 0 {
		page := int64(pi.PageSize)
		physTotal := int64(pi.PhysicalTotal) * page
		physAvail := int64(pi.PhysicalAvailable) * page
		commitLimit := int64(pi.CommitLimit) * page
		commitUsed := int64(pi.CommitTotal) * page
		if physTotal > 0 {
			l.MemTotal, l.MemUsed = physTotal, physTotal-physAvail
		}
		if swapTotal := commitLimit - physTotal; swapTotal > 0 {
			l.SwapTotal = swapTotal
			swapUsed := commitUsed - (physTotal - physAvail)
			if swapUsed < 0 {
				swapUsed = 0
			}
			if swapUsed > swapTotal {
				swapUsed = swapTotal
			}
			l.SwapUsed = swapUsed
		}
	}

	l.DiskTotal, l.DiskUsed = diskUsage()
	return l, nil
}
