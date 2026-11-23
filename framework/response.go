// response.go：HTTP/1.1 响应序列化（服务端，连接链路）。
//
// 响应写出策略（对齐 net/http 语义，尽量简单可靠）：
//   - 处理器设置了 Content-Length → 定长写出；
//   - 未定长且累计数据 ≤ maxBuffered(2KB) → 结束时自动定长（利于小 JSON 响应）；
//   - 超过或主动 Flush() → HTTP/1.1 转入 chunked 流式（HTTP/1.0 直接写+关连接）；
//   - HEAD 请求丢弃响应体但保留状态与长度信息；
//   - keep-alive 由请求方声明决定，响应头显式回写。
package framework

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxBuffered  = 2 * 1024 // 未知长度时的最大缓冲，超过即转 chunked
	serverName   = "framework/0.1"
	writeBufSize = 2 * 1024
)

// ErrWriteAfterHijack Hijack 之后不得再写 HTTP 响应。
var ErrWriteAfterHijack = errors.New("framework: write after hijack")

// responseWriter 连接链路的响应写出器。
// 实现 responseSink，供 *Context 驱动。
type responseWriter struct {
	conn         net.Conn
	req          *Request
	header       http.Header
	status       int
	wroteHeader  bool // 状态码已确定
	started      bool // 响应字节已开始写出（头不可再改）
	chunked      bool // 已进入 chunked 流式
	finished     bool
	hijacked     bool
	keepAlive    bool
	writeTimeout time.Duration

	buf  []byte // 未知长度时的累计缓冲
	size int64  // 响应体字节数（HEAD 也计数）
}

func newResponseWriter(conn net.Conn, req *Request, keepAlive bool, writeTimeout time.Duration) *responseWriter {
	return &responseWriter{
		conn:         conn,
		req:          req,
		header:       make(http.Header),
		keepAlive:    keepAlive,
		writeTimeout: writeTimeout,
	}
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader || w.started || w.hijacked {
		return
	}
	if code < 100 || code > 599 {
		code = http.StatusInternalServerError
	}
	w.status = code
	w.wroteHeader = true
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.hijacked || w.finished {
		if w.hijacked {
			return 0, ErrWriteAfterHijack
		}
		return 0, errors.New("framework: response already finished")
	}
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	w.size += int64(len(p))
	if w.req != nil && w.req.Method == http.MethodHead {
		return len(p), nil // 丢弃体，但保留长度计数
	}
	if w.chunked {
		return w.writeChunk(p)
	}
	if w.contentLengthKnown() {
		if err := w.ensureStarted(); err != nil {
			return 0, err
		}
		return w.rawWrite(p)
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) > maxBuffered {
		if err := w.beginChunked(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *responseWriter) Flush() {
	if w.hijacked || w.finished {
		return
	}
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.chunked {
		w.touchDeadline()
		return // 已流式直写，无需处理
	}
	if w.contentLengthKnown() {
		if err := w.ensureStarted(); err != nil {
			return
		}
		return
	}
	// 未知长度 → 转流式（HTTP/1.1 chunked；HTTP/1.0 写到关闭）
	if w.req != nil && w.req.http10 {
		if err := w.ensureStarted(); err != nil {
			return
		}
		if len(w.buf) > 0 {
			_, _ = w.rawWrite(w.buf)
			w.buf = w.buf[:0]
		}
		w.keepAlive = false
		return
	}
	if err := w.beginChunked(); err != nil {
		return
	}
}

// finish 结束响应：补写头/尾帧。Hijack 后为空操作。
func (w *responseWriter) Finish() error {
	if w.hijacked || w.finished {
		return nil
	}
	w.finished = true
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.chunked {
		// 终止块
		_, err := w.rawWrite([]byte("0\r\n\r\n"))
		return err
	}
	if !w.started {
		// 一次性写出（未知长度缓冲此刻已知长度）
		if w.header.Get("Content-Length") == "" {
			w.header.Set("Content-Length", strconv.FormatInt(w.sizeOrBufLen(), 10))
		}
		if err := w.writeHeaderBytes(); err != nil {
			return err
		}
		if w.req == nil || w.req.Method != http.MethodHead {
			if len(w.buf) > 0 {
				if _, err := w.rawWrite(w.buf); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// 已 started：定长流（CL 已写在头里），无尾帧
	return nil
}

// sizeOrBufLen HEAD 请求用计数长度；普通响应用缓冲长度。
func (w *responseWriter) sizeOrBufLen() int64 {
	if w.req != nil && w.req.Method == http.MethodHead {
		return w.size
	}
	return int64(len(w.buf))
}

func (w *responseWriter) contentLengthKnown() bool {
	return w.header.Get("Content-Length") != ""
}

// beginChunked 开始流式（写头 + 缓冲数据首个 chunk）。
func (w *responseWriter) beginChunked() error {
	w.chunked = true
	w.header.Del("Content-Length")
	if err := w.writeHeaderBytes(); err != nil {
		return err
	}
	if len(w.buf) > 0 {
		if _, err := w.writeChunk(w.buf); err != nil {
			return err
		}
		w.buf = w.buf[:0]
	}
	return nil
}

// ensureStarted 已定长模式：头先写出去。
func (w *responseWriter) ensureStarted() error {
	if w.started {
		return nil
	}
	w.started = true // writeHeaderBytes 内部会再置位，这里提前防递归
	return w.writeHeaderBytes()
}

// writeHeaderBytes 组装并写出状态行+头部，随后进入“已开始”状态。
func (w *responseWriter) writeHeaderBytes() error {
	var b strings.Builder
	b.WriteString(w.reqProto())
	b.WriteByte(' ')
	code := w.status
	b.WriteString(strconv.Itoa(code))
	b.WriteByte(' ')
	b.WriteString(http.StatusText(code))
	b.WriteString("\r\n")

	if w.header.Get("Date") == "" {
		w.header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	if w.header.Get("Server") == "" {
		w.header.Set("Server", serverName)
	}
	if w.chunked {
		w.header.Set("Transfer-Encoding", "chunked")
	} else {
		w.header.Del("Transfer-Encoding")
	}
	if w.header.Get("Connection") == "" {
		if w.keepAlive {
			w.header.Set("Connection", "keep-alive")
		} else {
			w.header.Set("Connection", "close")
		}
	}
	for k, vs := range w.header {
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\r\n")

	w.started = true
	w.touchDeadline()
	_, err := w.conn.Write([]byte(b.String()))
	return err
}

// writeChunk 写一个 chunk 帧。
func (w *responseWriter) writeChunk(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	frame := make([]byte, 0, len(p)+20)
	frame = append(frame, strconv.AppendInt([]byte{}, int64(len(p)), 16)...)
	frame = append(frame, '\r', '\n')
	frame = append(frame, p...)
	frame = append(frame, '\r', '\n')
	w.touchDeadline()
	_, err := w.conn.Write(frame)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *responseWriter) rawWrite(p []byte) (int, error) {
	w.touchDeadline()
	return w.conn.Write(p)
}

func (w *responseWriter) touchDeadline() {
	if w.writeTimeout > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	}
}

// Snapshot 返回未提交响应的体快照（未知长度缓冲窗口内）；
// 已 started/chunked/hijacked 返回 nil（不可捕获）。
func (w *responseWriter) Snapshot() []byte {
	if w.started || w.chunked || w.hijacked || w.finished {
		return nil
	}
	if w.contentLengthKnown() {
		return nil
	}
	return w.buf
}

// Rewrite 在缓冲窗口内替换响应体（gzip/etag 等后置改写用）。
// 成功返回 true；不可捕获时为无操作。
func (w *responseWriter) Rewrite(b []byte) bool {
	if w.Snapshot() == nil {
		return false
	}
	w.size = int64(len(b))
	w.buf = w.buf[:0]
	w.buf = append(w.buf, b...)
	return true
}

func (w *responseWriter) reqProto() string {
	if w.req != nil && w.req.Proto != "" {
		return w.req.Proto
	}
	return "HTTP/1.1"
}

// responseSink 实现（Context 侧查询）。

func (w *responseWriter) Status() int    { return w.status }
func (w *responseWriter) Size() int64    { return w.size }
func (w *responseWriter) Hijacked() bool { return w.hijacked }
func (w *responseWriter) MarkHijacked()  { w.hijacked = true }

// Hijack 移交原始连接（WebSocket 升级等）；此后响应不再写 HTTP 字节。
// 返回的 bufio.Reader 内含“头部之后已预读进缓冲”的残留字节，升级协议必须接管。
func (w *responseWriter) Hijack() (net.Conn, *bufio.Reader, error) {
	if w.hijacked {
		return nil, nil, errors.New("framework: connection already hijacked")
	}
	w.hijacked = true
	w.finished = true // 不再走 finish
	if w.req == nil {
		return w.conn, nil, errors.New("framework: hijack requires request context")
	}
	return w.req.rawConn, w.req.br, nil
}
