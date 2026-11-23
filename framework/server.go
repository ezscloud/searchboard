// server.go：HTTP/1.1 服务端。
//
// 组装关系：
//
//	Server ──use──> transport.Listener.Serve(serveConn)   （netpoll / std 两实现）
//	  │
//	  └─serveConn：bufio 读 → readRequest → Router.Dispatch → finish → keep-alive 循环
//
// 平台策略由 Config.Transport 控制：auto/netpoll（Windows 自动降级 std）或 std。
package framework

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ezscloud/searchboard/framework/transport"
	"golang.org/x/net/http2"
)

// Config 服务端配置。
type Config struct {
	Network string // "tcp"（默认）
	Addr    string // 如 "127.0.0.1:8080" / ":8080"

	// Transport："" / "auto" / "netpoll" / "std"。
	// auto=netpoll（仅 linux/darwin/bsd 实际生效）→ 其余平台自动 std。
	Transport string

	ReadHeaderTimeout time.Duration // 读请求头时限，默认 10s
	ReadTimeout       time.Duration // 请求体读取时限，0=不限
	WriteTimeout      time.Duration // 响应写出时限，0=不限
	IdleTimeout       time.Duration // keep-alive 空闲时限，默认 120s
	MaxHeaderBytes    int           // 默认 64KB
	MaxBodyBytes      int64         // 默认 32MB
	TLS               *tls.Config

	// DisableH2 关闭 HTTP/2（默认开启：TLS-ALPN 协商 h2 或 h2c 时由
	// golang.org/x/net/http2 提供完整多路复用服务）。
	DisableH2 bool

	// EnableH2C 开启明文 HTTP/2（prior-knowledge 前导串嗅探），默认关闭。
	EnableH2C bool

	// MaxConcurrentStreams 传给 http2.Server（0=按规范默认 ≥100）。
	MaxConcurrentStreams uint32

	// Logf 内部事件日志（启停、平台选择等）；nil=静默。
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	cfg := c
	if cfg.Network == "" {
		cfg.Network = "tcp"
	}
	if cfg.Transport == "" {
		cfg.Transport = "auto"
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 120 * time.Second
	}
	if cfg.MaxHeaderBytes == 0 {
		cfg.MaxHeaderBytes = defaultMaxHdr
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBody
	}
	return cfg
}

func (c Config) useNetpoll() bool {
	switch c.Transport {
	case "netpoll", "auto", "":
		return true
	case "std":
		return false
	default:
		return true
	}
}

// Server HTTP/1.1 服务端。
type Server struct {
	cfg    Config
	router *Router

	ln        transport.Listener
	lnMu      sync.Mutex
	started   bool
	serveDone chan error
	shutdown  atomic.Bool

	h2 *http2.Server // nil = HTTP/2 未启用

	mu      sync.Mutex
	conns   map[net.Conn]struct{} // 活跃连接（含已 Hijack 移交的）
	h2conns map[net.Conn]struct{} // 其中走 HTTP/2 的连接（Shutdown 定向发 GOAWAY）
}

// New 创建服务端。
func New(cfg Config) *Server {
	c := cfg.withDefaults()
	s := &Server{
		cfg:     c,
		router:  NewRouter(),
		conns:   map[net.Conn]struct{}{},
		h2conns: map[net.Conn]struct{}{},
	}
	// HTTP/2（默认启用；TLS-ALPN 与可选 h2c 两路接入）
	if !c.DisableH2 && (c.TLS != nil || c.EnableH2C) {
		s.h2 = s.newH2Server()
	}
	return s
}

// Router 暴露路由器（h3 挂载 / 分组注册等）。
func (s *Server) Router() *Router { return s.router }

// Use 全局中间件。
func (s *Server) Use(m ...Middleware) { s.router.Use(m...) }

// Handle 注册路由。
func (s *Server) Handle(method, path string, h HandlerFunc) error {
	return s.router.Handle(method, path, h)
}

// 方法族快捷注册。
func (s *Server) GET(path string, h HandlerFunc)     { s.router.GET(path, h) }
func (s *Server) POST(path string, h HandlerFunc)    { s.router.POST(path, h) }
func (s *Server) PUT(path string, h HandlerFunc)     { s.router.PUT(path, h) }
func (s *Server) DELETE(path string, h HandlerFunc)  { s.router.DELETE(path, h) }
func (s *Server) PATCH(path string, h HandlerFunc)   { s.router.PATCH(path, h) }
func (s *Server) HEAD(path string, h HandlerFunc)    { s.router.HEAD(path, h) }
func (s *Server) OPTIONS(path string, h HandlerFunc) { s.router.OPTIONS(path, h) }
func (s *Server) Any(path string, h HandlerFunc)     { s.router.Any(path, h) }

// Addr 监听地址（ListenAndServe 之后可用）。
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Start 创建监听并启动后台服务循环（非阻塞）。
// 返回 nil 后 Addr() 即可用；ctx 仅用于继承语义（停止由 Shutdown/Close 驱动）。
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.started {
		return errors.New("framework: server already started")
	}
	ln, err := transport.Listen(s.cfg.Network, s.cfg.Addr, &transport.Options{
		UseNetpoll: s.cfg.useNetpoll(),
		TLS:        s.tlsConfigForListen(),
		Logger:     s.cfg.Logf,
	})
	if err != nil {
		return err
	}
	s.ln = ln
	s.started = true
	s.serveDone = make(chan error, 1)
	go func() {
		s.logf("listening on %s (transport=%s)", ln.Addr(), transport.TransportName(s.cfg.useNetpoll()))
		s.serveDone <- ln.Serve(s.serveConn)
	}()
	return nil
}

// ListenAndServe 创建监听并阻塞服务（直至 Shutdown/Close）。
func (s *Server) ListenAndServe() error {
	if err := s.Start(context.Background()); err != nil {
		return err
	}
	return <-s.serveDone
}

// Run 监听并服务直至 ctx 取消，随后优雅停机（等待存量会话 ≤5s）。
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	select {
	case err := <-s.serveDone:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			select {
			case <-s.serveDone:
			case <-time.After(2 * time.Second):
			}
			return err
		}
		select {
		case e := <-s.serveDone:
			if e != nil && !errors.Is(e, transport.ErrListenerClosed) {
				return e
			}
		case <-time.After(2 * time.Second):
			return errors.New("framework: serve goroutine did not exit in time")
		}
		return nil
	}
}

// Shutdown 优雅停机：关监听 → 对 h2 连接发 GOAWAY（尽力）→ 等存量会话
// 结束 → 超时强制关闭连接。
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.shutdown.CompareAndSwap(false, true) {
		return nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	n := s.goawayH2Conns() // h2：通知客户端停发新流（现有流继续完成）
	if n > 0 {
		s.logf("shutdown: GOAWAY sent to %d http/2 conns", n)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.activeConns() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			s.closeAllConns()
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("framework: shutdown timeout, %d conns force-closed", s.activeConns())
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// goawayH2Conns 对所有活跃 h2 连接尽力发送 GOAWAY 帧（NO_ERROR），
// 返回发送条数。
//
// x/net/http2 无公开的优雅停机 API（同 Go 标准库，golang/go#22091），
// 本实现绕过其写循环手工构造 17 字节 GOAWAY：
//
//	Last-Stream-ID = 0x7FFFFFFF（客户端语义：已发起的流全部允许完成，
//	勿在此连接上发起新流），ErrCode = 0x0 NO_ERROR。
//
// 客户端（浏览器 / http2.Transport）收到后：现流等完成、新请求换连。
// 与 ServeConn 内部写帧的理论交错风险（同连接两个并发 Write）在 17 字节
// 单帧场景可忽略；写在已关闭连接上直接失败，同样无害。
func (s *Server) goawayH2Conns() int {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.h2conns))
	for c := range s.h2conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_, _ = c.Write(h2GoAwayFrame)
	}
	return len(conns)
}

// h2GoAwayFrame 手工构造的 GOAWAY 帧（RFC 9113 §6.8）。
//
// 9 字节帧头：Length=8, Type=0x7(GOAWAY), Flags=0, StreamID=0
// 8 字节载荷：Last-Stream-ID=0x7FFFFFFF, Error=0(NO_ERROR)
var h2GoAwayFrame = []byte{
	0x00, 0x00, 0x08, // Length = 8
	0x07,                   // Type = GOAWAY
	0x00,                   // Flags
	0x00, 0x00, 0x00, 0x00, // StreamID = 0
	0x7f, 0xff, 0xff, 0xff, // Last-Stream-ID = MaxUInt32/2 (MaxStreamID)
	0x00, 0x00, 0x00, 0x00, // Error = NO_ERROR
}

// markH2/unmarkH2 登记 h2 连接（ServeConn 执行期间持有，
// 供 Shutdown 定向发 GOAWAY，避免向 h1 连接写 h2 帧字节污染协议流）。
func (s *Server) markH2(c net.Conn) {
	s.mu.Lock()
	s.h2conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) unmarkH2(c net.Conn) {
	s.mu.Lock()
	delete(s.h2conns, c)
	s.mu.Unlock()
}

// ---------------------------------------------------------------- 内部

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

func (s *Server) trackConn(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrackConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) activeConns() int {
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	return n
}

func (s *Server) closeAllConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// tlsConfigForListen 组装监听用 TLS 配置：h2 启用时补 ALPN 协商列表。
func (s *Server) tlsConfigForListen() *tls.Config {
	if s.cfg.TLS == nil {
		return nil
	}
	if s.h2 != nil {
		return ensureH2TLSConfig(s.cfg.TLS)
	}
	return s.cfg.TLS
}

// serveConn 单连接会话：HTTP/2 分派 + HTTP/1.1 keep-alive 循环。
//
// 返回值：true = 连接已 Hijack 移交（传输层不得关闭）。
func (s *Server) serveConn(conn net.Conn) bool {
	s.trackConn(conn)
	defer s.untrackConn(conn)

	br := bufio.NewReaderSize(conn, 4096)

	// HTTP/2 分派：TLS-ALPN 协商 h2，或明文 h2c（前导串嗅探）。
	// 返回 true 表示会话已由 h2 服务完毕。
	if s.h2 != nil && s.dispatchH2(conn, br) {
		return false
	}

	for {
		// 两段超时：等下一请求首字节用 IdleTimeout（未设置则用 ReadHeaderTimeout），
		// 首字节到达后读头部改为 ReadHeaderTimeout。
		waitDeadline := s.cfg.ReadHeaderTimeout
		if s.cfg.IdleTimeout > 0 && s.cfg.IdleTimeout < waitDeadline {
			waitDeadline = s.cfg.IdleTimeout
		}
		if waitDeadline > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(waitDeadline))
		}
		if _, err := br.Peek(1); err != nil {
			return false // 空闲超时 / EOF / 关闭
		}
		if s.cfg.ReadHeaderTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadHeaderTimeout))
		}
		req, err := readRequest(br, conn, parseLimits{
			maxHeader: s.cfg.MaxHeaderBytes,
			maxBody:   s.cfg.MaxBodyBytes,
		})
		if err != nil {
			s.writeEarlyError(conn, req, err)
			return false
		}

		// 请求体阶段
		if s.cfg.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		keepAlive := !req.closeAfter
		resp := newResponseWriter(conn, req, keepAlive, s.cfg.WriteTimeout)
		ctx := NewContext(req, resp)
		s.router.Dispatch(ctx)
		ctx.CloseDone() // 请求生命周期结束（收尾兜底关闭）

		// Hijack：连接所有权移交（ws 升级等）
		if resp.Hijacked() {
			return true
		}
		if err := resp.Finish(); err != nil {
			return false
		}
		if !keepAlive || !resp.keepAlive {
			return false
		}
		// 排水剩余请求体（超出排水上限则弃用连接）
		if b, ok := req.Body.(*body); ok && !drainBody(b) {
			return false
		}
	}
}

// writeEarlyError 请求解析失败时尽力回一个错误响应。
func (s *Server) writeEarlyError(conn net.Conn, req *Request, err error) {
	code := 0
	switch {
	case errors.Is(err, ErrBadRequest), errors.Is(err, ErrChunkedBadFormat):
		code = http.StatusBadRequest
	case errors.Is(err, ErrHeaderTooLarge):
		code = http.StatusRequestHeaderFieldsTooLarge
	case errors.Is(err, ErrRequestLineToo):
		code = http.StatusRequestURITooLong // 414
	case errors.Is(err, ErrBadProtocol):
		code = http.StatusHTTPVersionNotSupported
	case errors.Is(err, ErrBodyTooLarge):
		code = http.StatusRequestEntityTooLarge
	default:
		// EOF/超时/连接错误：无可写对象
		return
	}
	proto := "HTTP/1.1"
	if req != nil && req.Proto != "" {
		proto = req.Proto
	}
	msg := http.StatusText(code)
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write([]byte(proto + " " + itoa(code) + " " + msg + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + itoa(len(msg)+1) + "\r\n" +
		"Connection: close\r\n\r\n" + msg + "\n"))
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
