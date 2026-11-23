// std_listener.go：基于标准库 net 的监听实现。
//
// 本文件不设构建标签，在所有平台编译——既作为 Windows 等不支持 netpoll 平台的
// 唯一实现，也作为 Unix 平台上未启用 netpoll 时的回退实现。
package transport

import (
	"crypto/tls"
	"net"
	"sync"
)

// stdListener 标准库实现：accept 循环 + 每连接一个 goroutine 运行 session。
type stdListener struct {
	ln      net.Listener
	opts    *Options
	session func(net.Conn) bool

	closeOnce sync.Once
	closeErr  error
}

func newStdListener(network, addr string, opts *Options) (*stdListener, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return &stdListener{ln: ln, opts: opts}, nil
}

// Serve implements Listener：阻塞 accept，直到 Close。
func (l *stdListener) Serve(session func(net.Conn) bool) error {
	l.session = session
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.isClosed(err) {
				return ErrListenerClosed
			}
			return err
		}
		go l.run(conn)
	}
}

// run 在独立 goroutine 中运行单条连接的 session。
// session 返回 false（常规结束）时关闭连接；返回 true 表示连接已移交。
func (l *stdListener) run(conn net.Conn) {
	keep := false
	defer func() {
		if r := recover(); r != nil && l.opts.logEnabled() {
			l.opts.logf("webfw/transport: session panic (recovered): %v", r)
		}
		if !keep {
			_ = conn.Close()
		}
	}()
	if l.opts != nil && l.opts.TLS != nil {
		conn = tls.Server(conn, l.opts.TLS)
	}
	keep = l.session(conn)
}

func (l *stdListener) Close() error {
	l.closeOnce.Do(func() { l.closeErr = l.ln.Close() })
	return l.closeErr
}

func (l *stdListener) Addr() net.Addr { return l.ln.Addr() }

func (l *stdListener) isClosed(err error) bool {
	return err != nil && l.closeErr != nil // closeErr 只在 Close 后非 nil
}
