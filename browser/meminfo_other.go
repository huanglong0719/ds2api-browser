//go:build !windows

package browser

// memInfo 系统物理内存信息（诊断用）
type memInfo struct {
	TotalMB int
	AvailMB int
	Load    int
}

// windowsMemoryInfo 在非 Windows 平台（如 Linux CI）返回 nil，
// 诊断时跳过内存信息，保证跨平台编译通过。
func windowsMemoryInfo() *memInfo {
	return nil
}
