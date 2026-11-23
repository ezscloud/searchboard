// limiter/cpu.go：进程/系统 CPU 时间读取的平台分派。
//
// Linux：/proc/self/stat（utime+stime，进程口径）；
// Windows：GetSystemTimes（整机 busy 口径，见 cpu_windows.go 注释）；
// 其它平台：降级为 0（限流仅靠延迟因子，仍有效）。
package limiter

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	cpuOnce sync.Once
	cpuKind string
)

func procSelfCPUTime() time.Duration {
	switch detect() {
	case "linux":
		return procStat()
	case "windows":
		return windowsCPUTime() // 整机 busy 口径（见 cpu_windows.go 注释）
	default:
		return 0
	}
}

func detect() string {
	cpuOnce.Do(func() {
		switch runtime.GOOS {
		case "linux":
			cpuKind = "linux"
		case "windows":
			cpuKind = "windows"
		default:
			cpuKind = "none"
		}
	})
	return cpuKind
}

// procStat Linux /proc/self/stat（utime+stime，CLK_TCK=100）。
func procStat() time.Duration {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 <= len(s) {
		s = s[i+2:] // 跳过 pid (comm)
	}
	fields := strings.Fields(s)
	if len(fields) < 13 {
		return 0
	}
	utime, err1 := strconv.ParseFloat(fields[11], 64)
	stime, err2 := strconv.ParseFloat(fields[12], 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return time.Duration((utime + stime) * 10 * float64(time.Millisecond))
}
