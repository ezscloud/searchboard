// context.go：*Context 是单次请求的处理器上下文。
//
// 同一上下文服务两条链路：
//   - 连接链路（HTTP/1.1 over transport）：sink 为 *responseWriter，
//     支持 Hijack（WebSocket 升级等）；
//   - HTTP/3-QUIC 链路：sink 为 stdSink（net/http.ResponseWriter），
//     Hijack 不可用（WS over H3 属 WebTransport 范畴，v1 不支持）。
//
// 中间件采用经典 Next 语义：handlers = [中间件..., 路由处理器]，
// 中间件内调用 c.Next() 继续链；不调用即短路。
package framework

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/ezscloud/searchboard/framework/jsonx"
)

// HandlerFunc 是处理器签名。
type HandlerFunc func(*Context)

// Middleware 与 HandlerFunc 同签名（语义约定：续链时调用 c.Next()）。
type Middleware = HandlerFunc

// abortIndex 置于链末之后，使 Next 循环终止。
const abortIndex = math.MaxInt32

// responseSink 是 Context 的响应出口抽象。
//
// 方法全导出：外部包（如 ext/gzip 的包装型 sink）可实现后经
// Context.WithSink 注入——接口类型本身不导出，但实现值可传入。
type responseSink interface {
	Header() http.Header
	WriteHeader(int)
	Write([]byte) (int, error)
	Flush()
	Status() int
	Size() int64
	Hijacked() bool
	MarkHijacked()
	// Snapshot 未提交响应体的快照（未知长度缓冲窗口内；已流式为 nil）。
	Snapshot() []byte
	// Rewrite 缓冲窗口内替换响应体（gzip/etag 等后置改写用）。
	Rewrite([]byte) bool
	// Finish 出口收尾（终止块/尾帧）。
	Finish() error
}

// Context 处理器上下文。
type Context struct {
	Request *Request
	Params  map[string]string // 路由参数（:name / *name 捕获）

	sink  responseSink
	store map[string]any

	handlers []HandlerFunc
	index    int

	// 请求生命周期信号（SSE 感知断开；惰性创建）
	done     chan struct{}
	doneOnce sync.Once
}

// NewContext 组装上下文（sink 由链路提供）。
func NewContext(req *Request, sink responseSink) *Context {
	return &Context{Request: req, sink: sink}
}

// ---------------------------------------------------------------------------
// 中间件链（经典 Next 语义）

// Next 执行链上下一个处理器；链末是路由处理器。
// Abort 会把游标推到链末，循环随之终止。
func (c *Context) Next() {
	for c.index < len(c.handlers) {
		h := c.handlers[c.index]
		c.index++
		h(c)
	}
}

// Abort 终止后续链（当前处理器继续执行完）。
func (c *Context) Abort() { c.index = abortIndex }

// Aborted 链是否已被终止。
func (c *Context) Aborted() bool { return c.index >= len(c.handlers) }

// dispatch 内部：装入链并驱动。
func (c *Context) dispatch(handlers []HandlerFunc) {
	c.handlers = handlers
	c.index = 0
	if len(handlers) == 0 {
		return
	}
	c.Next()
}

// ---------------------------------------------------------------------------
// 请求侧

// Param 取路由参数。
func (c *Context) Param(name string) string { return c.Params[name] }

// Query 取查询参数（首个值）。
func (c *Context) Query(name string) string {
	if c.Request == nil || c.Request.Query == nil {
		return ""
	}
	return c.Request.Query.Get(name)
}

// DefaultQuery 取查询参数，缺省回退。
func (c *Context) DefaultQuery(name, def string) string {
	if v := c.Query(name); v != "" {
		return v
	}
	return def
}

// Header 取请求头。
func (c *Context) Header(key string) string {
	return c.Request.Header.Get(key)
}

// Body 请求体读取器（无体为 nil）。
func (c *Context) Body() io.Reader { return c.Request.Body }

// ReadBody 读取请求体（≤ n 字节）。
func (c *Context) ReadBody(n int64) ([]byte, error) {
	if c.Request.Body == nil {
		return []byte{}, nil
	}
	return io.ReadAll(io.LimitReader(c.Request.Body, n))
}

// BindJSON 以 sonic 将请求体反序列化到 v。
func (c *Context) BindJSON(v any) error {
	b, err := c.ReadBody(32 << 20)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errors.New("framework: empty json body")
	}
	return jsonx.Unmarshal(b, v)
}

// ClientIP 客户端地址（不含端口）。
func (c *Context) ClientIP() string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// IsTLS 当前请求是否来自加密链路（连接链路未维护；H3 恒 true）。
func (c *Context) IsTLS() bool { return c.Request.Proto == "HTTP/3" }

// ---------------------------------------------------------------------------
// 响应侧

// RespHeader 响应头。
func (c *Context) RespHeader() http.Header { return c.sink.Header() }

// SetRespHeader 设置响应头。
func (c *Context) SetRespHeader(key, value string) { c.sink.Header().Set(key, value) }

// Status 设置状态码。
func (c *Context) Status(code int) { c.sink.WriteHeader(code) }

// CurrentStatus 已写状态码。
func (c *Context) CurrentStatus() int { return c.sink.Status() }

// ResponseSize 已写字节数。
func (c *Context) ResponseSize() int64 { return c.sink.Size() }

// ResponseSnapshot 未提交响应体快照（缓冲窗口内；流式响应为 nil）。
// gzip/etag 等中间件用于后置改写。
func (c *Context) ResponseSnapshot() []byte { return c.sink.Snapshot() }

// ReplaceBody 缓冲窗口内替换响应体；不可捕获时无操作并返回 false。
func (c *Context) ReplaceBody(b []byte) bool { return c.sink.Rewrite(b) }

// CurrentSink 当前响应出口（供 gzip 流式管道等包装型中间件取得原出口）。
//
// 返回值为不透明句柄：包装中间件经 WithSink 替换出口后，处理器写路径
// 均经过新出口；收尾用 RestoreSink 恢复。
func (c *Context) CurrentSink() ResponseSinkHandle { return ResponseSinkHandle{sink: c.sink} }

// ResponseSinkHandle 响应出口句柄（不透明；包装型中间件的透传操作面）。
type ResponseSinkHandle struct{ sink responseSink }

// RestoreSink 恢复为指定出口句柄（gzip 收尾用）。
func (c *Context) RestoreSink(h ResponseSinkHandle) { c.sink = h.sink }

// WithSink 替换响应出口（gzip 流式压缩等中间件用），返回被替换的出口句柄。
func (c *Context) WithSink(s responseSink) ResponseSinkHandle {
	old := ResponseSinkHandle{sink: c.sink}
	c.sink = s
	return old
}

// ---- 句柄操作（均透传至被包装的出口） ----

// Header 响应头。
func (h ResponseSinkHandle) Header() http.Header { return h.sink.Header() }

// WriteHeader 设置状态码。
func (h ResponseSinkHandle) WriteHeader(code int) { h.sink.WriteHeader(code) }

// Write 写响应体字节。
func (h ResponseSinkHandle) Write(p []byte) (int, error) { return h.sink.Write(p) }

// Flush 立即下发。
func (h ResponseSinkHandle) Flush() { h.sink.Flush() }

// Status 已写状态码。
func (h ResponseSinkHandle) Status() int { return h.sink.Status() }

// Size 已写字节数。
func (h ResponseSinkHandle) Size() int64 { return h.sink.Size() }

// Hijacked 连接是否已被接管。
func (h ResponseSinkHandle) Hijacked() bool { return h.sink.Hijacked() }

// MarkHijacked 标记接管。
func (h ResponseSinkHandle) MarkHijacked() { h.sink.MarkHijacked() }

// Snapshot 未提交响应体快照（缓冲窗口内）。
func (h ResponseSinkHandle) Snapshot() []byte { return h.sink.Snapshot() }

// Rewrite 缓冲窗口内替换响应体。
func (h ResponseSinkHandle) Rewrite(b []byte) bool { return h.sink.Rewrite(b) }

// Finish 驱动出口收尾。
func (h ResponseSinkHandle) Finish() error { return h.sink.Finish() }

// Write 写响应体。
func (c *Context) Write(p []byte) (int, error) { return c.sink.Write(p) }

// WriteString 写字符串响应体。
func (c *Context) WriteString(s string) (int, error) { return c.sink.Write([]byte(s)) }

// Flush 立即下发（SSE/流式）。
func (c *Context) Flush() { c.sink.Flush() }

// JSON 以 sonic 序列化并输出 JSON 响应。
func (c *Context) JSON(code int, v any) {
	c.sink.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.sink.WriteHeader(code)
	_, _ = c.sink.Write(jsonx.MarshalNoFail(v))
}

// String 输出文本响应（支持 fmt 占位符）。
func (c *Context) String(code int, format string, args ...any) {
	c.sink.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.sink.WriteHeader(code)
	c.sink.Write([]byte(strf(format, args...)))
}

// Data 输出原始字节响应。
func (c *Context) Data(code int, contentType string, b []byte) {
	if contentType != "" {
		c.sink.Header().Set("Content-Type", contentType)
	}
	c.sink.WriteHeader(code)
	_, _ = c.sink.Write(b)
}

// HTML 输出 HTML 响应。
func (c *Context) HTML(code int, html string) {
	c.sink.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.sink.WriteHeader(code)
	_, _ = c.sink.Write([]byte(html))
}

// Done 请求生命周期结束信号（SSE 长响应处理器与收尾协作）。
//
// 语义：请求会话收尾时关闭。长响应处理器感知客户端断开必须依靠写失败
// （定期写心跳注释行 / 推送失败即退出）——不提供底层 Read 探测：
// 连接上的字节归下一请求解析所有，任何探测读都会造成管线错位。
func (c *Context) Done() <-chan struct{} {
	c.doneOnce.Do(func() {
		c.done = make(chan struct{})
	})
	return c.done
}

// CloseDone 关闭请求生命周期信号（服务器会话循环收尾调用；测试慎用）。
func (c *Context) CloseDone() {
	c.doneOnce.Do(func() {
		c.done = make(chan struct{})
	})
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Redirect 30x 重定向。
func (c *Context) Redirect(code int, location string) {
	if code < 300 || code > 399 {
		code = http.StatusFound
	}
	c.sink.Header().Set("Location", location)
	c.sink.WriteHeader(code)
}

// Error 输出简单错误响应（msg 做 HTML 转义）。
func (c *Context) Error(code int, msg string) {
	c.String(code, "%s", htmlEscape(msg))
}

// ---------------------------------------------------------------------------
// 存取值（中间件间共享）

func (c *Context) Set(key string, v any) {
	if c.store == nil {
		c.store = map[string]any{}
	}
	c.store[key] = v
}

func (c *Context) Get(key string) (any, bool) {
	v, ok := c.store[key]
	return v, ok
}

func (c *Context) GetString(key string) string {
	if v, ok := c.store[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Hijack（仅连接链路）

// RawConn 返回底层传输连接（H3 链路为 nil；可用于判断是否支持升级）。
func (c *Context) RawConn() net.Conn {
	if req := c.Request; req != nil {
		return req.rawConn
	}
	return nil
}

// HijackResult 是移交结果：连接 + 头部解析后残留的预读缓冲。
type HijackResult struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

// Hijack 移交连接（WebSocket 升级用；H3 链路报错）。
func (c *Context) Hijack() (*HijackResult, error) {
	if h, ok := c.sink.(interface {
		Hijack() (net.Conn, *bufio.Reader, error)
	}); ok {
		conn, br, err := h.Hijack()
		if err != nil {
			return nil, err
		}
		c.sink.MarkHijacked()
		return &HijackResult{Conn: conn, Reader: br}, nil
	}
	return nil, errors.New("framework: hijack unsupported on this chain")
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// strf 供 String/日志格式化。
func strf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
