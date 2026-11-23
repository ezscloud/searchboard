// h2.go：HTTP/2 支持（委托 golang.org/x/net/http2，完整多路复用实现）。
//
// 两条接入路径：
//  1. TLS-ALPN：TLS 握手协商出 "h2" → http2.Server.ServeConn 接管整条连接；
//  2. h2c（明文 prior-knowledge，需显式开启 EnableH2C）：嗅探 24 字节客户端
//     前导串，命中则接管（SawClientPreface 回放已预读字节）。
//
// 复用方式与 hertz-contrib/http2 同类：框架提供协议扩展点，h2 实现复用
// StdHandler()（同一套路由）。WebSocket 仍走 HTTP/1.1（h1 ALPN）。
//
// 已知限制：Hijack 在 h2 链路不可用（多路复用下的连接接管语义冲突，
// net/http 标准库与 Hertz 同样不支持；WS 自动走 h1 ALPN 路径）。
// 停机：Shutdown 会对活跃 h2 连接尽力发送 GOAWAY(NO_ERROR)——客户端
// 停发新流、现流完成后再关闭（有限优雅，见 server.go goawayH2Conns）。
package framework

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"time"

	"golang.org/x/net/http2"
)

// clientPreface HTTP/2 明文连接前导（RFC 9113 §3.1）。
const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// ensureH2TLSConfig 给服务端 TLS 配置补 ALPN（h2 + http/1.1）。
func ensureH2TLSConfig(cfg *tls.Config) *tls.Config {
	out := cfg.Clone()
	has := func(list []string, v string) bool {
		for _, s := range list {
			if s == v {
				return true
			}
		}
		return false
	}
	if !has(out.NextProtos, "h2") {
		out.NextProtos = append(out.NextProtos, "h2")
	}
	if !has(out.NextProtos, "http/1.1") {
		out.NextProtos = append(out.NextProtos, "http/1.1")
	}
	return out
}

// newH2Server 构造 x/net http2.Server（挂本服务路由的 std 适配）。
func (s *Server) newH2Server() *http2.Server {
	h := &http2.Server{
		IdleTimeout: s.cfg.IdleTimeout,
	}
	if s.cfg.MaxConcurrentStreams > 0 {
		h.MaxConcurrentStreams = s.cfg.MaxConcurrentStreams
	}
	return h
}

// dispatchH2 尝试把连接分派给 HTTP/2。
//
// 返回 true 表示连接已被 h2 接管且服务完成（session 契约内同步阻塞运行，
// 返回后由传输层统一关闭连接）。返回 false 表示继续走 HTTP/1.1。
func (s *Server) dispatchH2(conn net.Conn, br *bufio.Reader) bool {
	if s.h2 == nil {
		return false
	}
	opts := &http2.ServeConnOpts{Handler: s.StdHandler()}

	if tc, ok := conn.(*tls.Conn); ok {
		// TLS-ALPN：握手后按协商结果分派
		dl := s.cfg.ReadHeaderTimeout
		if dl <= 0 {
			dl = 10 * time.Second
		}
		_ = tc.SetDeadline(time.Now().Add(dl))
		if err := tc.HandshakeContext(context.Background()); err != nil {
			return true // 握手失败：连接已不可用，直接结束会话
		}
		_ = tc.SetDeadline(time.Time{})
		if tc.ConnectionState().NegotiatedProtocol != "h2" {
			return false // h1：继续走 HTTP/1.1 循环
		}
		s.markH2(tc)
		defer s.unmarkH2(tc)
		s.h2.ServeConn(tc, opts)
		return true
	}

	// h2c：明文前导嗅探（仅 EnableH2C）。
	// 两段式：先 Peek 3 字节确认 "PRI" 前缀，再 Peek 完整 24 字节前导，
	// 避免普通/超短 h1 请求在 Peek(24) 上阻塞到超时。
	// 命中后必须用只回放缓冲数据的 prefaceConn，避免 io.Reader 分支
	// 引起 http2 帧读取错位（FRAME_SIZE_ERROR 根因）。
	if s.cfg.EnableH2C && br != nil {
		if p3, err := br.Peek(3); err == nil && string(p3) == "PRI" {
			if pre, err := br.Peek(len(clientPreface)); err == nil && string(pre) == clientPreface {
				// 消耗掉已确认的前导，只把"缓冲里剩余的帧字节"回放给 http2
				_, _ = br.Discard(len(clientPreface))
				hc := &prefaceConn{Conn: conn, br: br}
				s.markH2(hc)
				defer s.unmarkH2(hc)
				s.h2.ServeConn(hc,
					&http2.ServeConnOpts{Handler: s.StdHandler(), SawClientPreface: true})
				return true
			}
		}
	}
	return false
}

// prefaceConn 严格回放 bufio 缓冲剩余字节（h2c 用）。
//
// 关键：io.Reader 分支的 Read 会一次读满 p（补充新数据），当上层用
// io.ReadFull(24) 语义时会把不属于前导的 SETTINGS 帧字节也吞掉，
// 导致 http2 帧解析错位（FRAME_SIZE_ERROR）。本实现保证回放只来自
// 缓冲存量，缓冲耗尽后直接透传底层连接。
type prefaceConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *prefaceConn) Read(p []byte) (int, error) {
	if c.br != nil {
		n := c.br.Buffered()
		if n == 0 {
			c.br = nil // 缓冲耗尽：此后直连底层
		} else {
			if n > len(p) {
				n = len(p)
			}
			return c.br.Read(p[:n])
		}
	}
	return c.Conn.Read(p)
}
