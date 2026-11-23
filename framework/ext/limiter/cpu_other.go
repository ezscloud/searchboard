// cpu_other.go：非 Windows 平台的 windowsCPUTime 符号兜底。
//
// linux 运行时走 procStat（不会调用本函数）；darwin/bsd 无系统级 CPU
// 读取实现（sysctl/mach 调用 v1 不做），调用时返回 0——限流退化为仅
// 延迟因子，仍然有效。
//
//go:build !windows

package limiter

import "time"

// windowsCPUTime 非 Windows 兜底（编译期符号存在；运行时 linux 不会走到）。
func windowsCPUTime() time.Duration { return 0 }
