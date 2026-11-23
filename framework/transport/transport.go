// Package transport 是 framework 的传输层抽象：
//
//	┌──────────── HTTP / WebSocket / RPC 协议层（只面向 net.Conn 语义） ────────────┐
//	│                                                                              │
//	│   Listener.Serve(session func(net.Conn))        Dial(network, addr, timeout) │
//	└────────┬───────────────────────────────────────────────────────┬──────────────┘
//	         │                                                       │
//	   ┌─────┴────────────────────┐                    ┌─────────────┴─────────────┐
//	   │ linux / darwin / *bsd    │                    │ windows 及其它平台         │
//	   │ cloudwego/netpoll v0.7.5 │                    │ 标准库 net                 │
//	   │ epoll/kqueue 事件驱动    │                    │ goroutine-per-conn         │
//	   └──────────────────────────┘                    └───────────────────────────┘
//
// 平台策略（用户约定）：netpoll 在 Linux 与 macOS 可用；Windows 下 netpoll 官方
// 实现为空桩（NewEventLoop/CreateListener 均返回 nil），因此 Windows 一律回退
// 标准库 net——上层协议代码完全无感知。
//
// 两种实现共有的关键约定（上层依赖这两条）：
//
//  1. session 在传输层管理的独立执行流中运行（std：每连接一个 goroutine；
//     netpoll：OnRequest 任务 goroutine），session 阻塞读完全合法；
//  2. netpoll.Connection 完整实现了 net.Conn（含 Read/Write/Deadline），
//     因此协议层只依赖 net.Conn 接口，两平台行为一致。
package transport

import (
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// ErrListenerClosed 监听器已关闭（Serve 将返回此错误或 nil）。
var ErrListenerClosed = errors.New("webfw/transport: listener closed")

// Options 传输层选项。
type Options struct {
	// UseNetpoll 在支持的平台上启用 netpoll 事件驱动实现。
	// Options 为 nil 或该字段为 false 时使用标准库实现。
	UseNetpoll bool

	// TLS 服务端配置：非 nil 时每条接入连接在交给 session 前包裹 tls.Server。
	// （对 QUIC/HTTP3 无效，那条链路由 webfw/h3 单独处理。）
	TLS *tls.Config

	// Logger 输出传输层事件日志；nil 表示静默（默认）。
	Logger func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o != nil && o.Logger != nil {
		o.Logger(format, args...)
	}
}

func (o *Options) logEnabled() bool { return o != nil && o.Logger != nil }

// Listener 统一服务端监听器。
//
// Serve 阻塞运行；为每条接入连接调用 session（在传输层管理的执行流中运行，
// 可能是独立 goroutine）。session 必须自循环直至连接结束（EOF/超时/主动关闭）
// 后返回；返回值约定：
//
//	false：常规结束，传输层负责关闭底层连接；
//	true ：连接所有权已移交（如 WebSocket 升级给业务 hub），传输层不得关闭，
//	       由接管方自行关闭。
//
// session 内部发生 panic 会被捕获并关闭该连接，不会拖垮其它连接。
//
// Close 关闭监听器并停止接入；已运行的 session 不受影响，由上层
// （framework.Server.Shutdown）负责收尾。Serve 在 Close 之后返回
// ErrListenerClosed。
type Listener interface {
	Serve(session func(net.Conn) bool) error
	Close() error
	Addr() net.Addr
}

// Listen 创建监听器。
//
// network 当前支持 "tcp"/"tcp4"/"tcp6"（netpoll 同此限制）；
// addr 形如 "127.0.0.1:8080" 或 ":8080"（端口 0 由内核分配，
// 创建后用 Addr() 查询实际端口）。
//
// 平台选择规则：
//   - opts.UseNetpoll 为真且运行平台受支持（linux/darwin/*bsd）→ netpoll；
//   - 其余情况（包括所有 Windows）→ 标准库 net。
func Listen(network, addr string, opts *Options) (Listener, error) {
	usePoll := opts != nil && opts.UseNetpoll && netpollAvailable()
	return newListener(network, addr, opts, usePoll)
}

// Dial 建立客户端连接（带超时）。
//
// 与 Listen 同一平台策略：netpoll 平台优先用 netpoll Dialer（返回的
// Connection 同样满足 net.Conn），否则 net.DialTimeout。TLS 客户端包裹
// 由调用方自行完成（tls.Client(conn, cfg)）。
func Dial(network, addr string, timeout time.Duration) (net.Conn, error) {
	return newDial(network, addr, timeout)
}

// TransportName 返回当前平台默认（未强制指定时）将采用的实现名，便于日志与演示。
func TransportName(useNetpoll bool) string {
	if useNetpoll && netpollAvailable() {
		return "netpoll"
	}
	return "std"
}

// netpollAvailable 由平台分派文件定义：
// netpoll 平台返回 true（并通过 newListener/dialNetpoll 提供实现），
// 其余平台（Windows 等）返回 false。
var netpollAvailable func() bool
