// cpu_windows.go：GetSystemTimes 读取系统 busy CPU 时间。
//
// 口径说明：返回整机 busy（kernel+user−idle）。本框架目标场景（云电脑
// 单机单进程）整机≈进程；多进程宿主上该口径偏保守（更早限流），符合
// 自适应保护语义。需要严格进程口径可换 GetProcessTimes 自行扩展。
//
//go:build windows

package limiter

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	modkernel32        = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes = modkernel32.NewProc("GetSystemTimes")
)

// windowsCPUTime 系统 busy 时间（纳秒）。
func windowsCPUTime() time.Duration {
	var idle, kernel, user syscall.Filetime
	r1, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r1 == 0 {
		return 0
	}
	busy := (kernel.Nanoseconds() + user.Nanoseconds()) - idle.Nanoseconds()
	if busy < 0 {
		busy = 0
	}
	return time.Duration(busy)
}
