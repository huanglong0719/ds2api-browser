//go:build windows

package browser

import (
	"syscall"
	"unsafe"
)

// memInfo 系统物理内存信息（诊断用）
type memInfo struct {
	TotalMB int
	AvailMB int
	Load    int
}

// memoryStatusEx 对应 Windows MEMORYSTATUSEX 结构体
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

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	globalMemStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// windowsMemoryInfo 获取 Windows 系统物理内存信息（标准库 syscall 直接调用 GlobalMemoryStatusEx，
// 不依赖第三方 x/sys，避免 vendor 模式缺少依赖）。仅 Windows 编译。
func windowsMemoryInfo() *memInfo {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := globalMemStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return nil
	}
	return &memInfo{
		TotalMB: int(m.TotalPhys / (1024 * 1024)),
		AvailMB: int(m.AvailPhys / (1024 * 1024)),
		Load:    int(m.MemoryLoad),
	}
}
