// netpoll_impl.go：netpoll 事件驱动实现（Linux/macOS/*BSD）。
//
// 执行模型：netpoll 的 OnRequest 回调在每连接独立的任务 goroutine 中运行，
// 且同一连接同时最多只有一个 OnRequest 在跑（processing 锁保证）。
// 因此首包到达时在 OnRequest 里直接运行整条 session（阻塞读完全合法，
// netpoll.Connection 实现了完整 net.Conn 语义），session 结束即关闭连接。
//
// 构建标签与 netpoll v0.7.5 自身的支持面（netpoll_unix.go）保持一致。
//
//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/netpoll"
)

func init() { netpollAvailable = func() bool { return true } }

// newListener 平台内分派：usePoll 为真走 netpoll，否则回退标准库。
func newListener(network, addr string, opts *Options, usePoll bool) (Listener, error) {
	if usePoll {
		return newNetpollListener(network, addr, opts)
	}
	return newStdListener(network, addr, opts)
}

// newDial 平台内拨号分派：本平台（unix 系）默认使用 netpoll。
func newDial(network, addr string, timeout time.Duration) (net.Conn, error) {
	return dialNetpoll(network, addr, timeout)
}

// dialNetpoll 客户端拨号：netpoll Dialer 返回的 Connection 满足 net.Conn。
//
// 包装为 eofConn：netpoll 在对端关闭时返回自家的 ErrEOF（syscall.Errno），
// 归一化为 io.EOF，使上层「读到 EOF」判断在两条传输实现上语义一致。
func dialNetpoll(network, addr string, timeout time.Duration) (net.Conn, error) {
	d := netpoll.NewDialer()
	conn, err := d.DialConnection(network, addr, timeout)
	if err != nil {
		return nil, err
	}
	return eofConn{conn}, nil
}

// eofConn 归一化 EOF 错误的连接包装。
type eofConn struct{ net.Conn }

func (c eofConn) Read(p []byte) (n int, err error) {
	n, err = c.Conn.Read(p)
	if err != nil && n == 0 && errors.Is(err, netpoll.ErrEOF) {
		err = io.EOF
	}
	return n, err
}

// netpollListener 基于 netpoll EventLoop 的监听器。
type netpollListener struct {
	ln        netpoll.Listener
	opts      *Options
	el        netpoll.EventLoop
	session   func(net.Conn) bool
	conns     sync.Map // netpoll.Connection -> struct{}：会话去重
	serving   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func newNetpollListener(network, addr string, opts *Options) (*netpollListener, error) {
	ln, err := netpoll.CreateListener(network, addr)
	if err != nil {
		return nil, err
	}
	l := &netpollListener{ln: ln, opts: opts}
	if opts.logEnabled() {
		opts.logf("webfw/transport: netpoll listener on %s (%s)", ln.Addr().String(), network)
	}
	return l, nil
}

// Serve implements Listener：阻塞直至 Close。
func (l *netpollListener) Serve(session func(net.Conn) bool) error {
	l.session = session
	l.serving.Store(true)
	el, err := netpoll.NewEventLoop(l.onRequest)
	if err != nil {
		return err
	}
	l.el = el
	return el.Serve(l.ln)
}

// onRequest 是 netpoll 的连接回调：首包触发时同步运行整条 session。
//
// 会话存活期间 netpoll 的 processing 锁由本任务持有，后续数据不会重入本函数
// （数据到达直接进入输入缓冲区并唤醒 session 的阻塞读）。
func (l *netpollListener) onRequest(ctx context.Context, nc netpoll.Connection) error {
	if _, dup := l.conns.LoadOrStore(nc, struct{}{}); dup {
		// 会话已启动（或已结束）：不再读取缓冲区，避免与 session 竞争。
		return nil
	}
	l.run(nc)
	return nil
}

// run 运行单条连接的 session；常规结束即清理并确保连接关闭。
// 返回 true（连接已移交）时不关闭、但从登记表中摘除。
func (l *netpollListener) run(nc netpoll.Connection) {
	keep := false
	defer func() {
		if r := recover(); r != nil && l.opts.logEnabled() {
			l.opts.logf("webfw/transport: session panic (recovered): %v", r)
		}
		l.conns.Delete(nc)
		if !keep {
			_ = nc.Close()
		}
	}()
	conn := net.Conn(nc)
	if l.opts != nil && l.opts.TLS != nil {
		conn = tls.Server(conn, l.opts.TLS)
	}
	keep = l.session(conn)
}

// Close implements Listener：关闭监听与空闲连接；活跃 session 由上层收尾。
func (l *netpollListener) Close() error {
	l.closeOnce.Do(func() {
		if l.serving.Load() && l.el != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := l.el.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				l.closeErr = err
			}
			return
		}
		l.closeErr = l.ln.Close()
	})
	return l.closeErr
}

func (l *netpollListener) Addr() net.Addr { return l.ln.Addr() }
