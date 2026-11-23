// dispatch_other.go：不支持 netpoll 的平台（Windows 等）的分派。
//
// netpoll v0.7.5 在 Windows 下仅有保证可编译的空桩（NewEventLoop /
// CreateListener / NewDialer 一律返回 nil），运行期不可用；因此这些平台
// 固定走标准库 net 实现，上层协议代码无感知。
//
//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package transport

import (
	"net"
	"time"
)

func init() { netpollAvailable = func() bool { return false } }

// newListener 永远使用标准库实现。
func newListener(network, addr string, opts *Options, usePoll bool) (Listener, error) {
	return newStdListener(network, addr, opts)
}

// newDial 标准库拨号。
func newDial(network, addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}
