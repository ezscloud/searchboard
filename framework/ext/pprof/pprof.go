// Package pprof 运行时剖析端点（标准库 net/http/pprof 的 framework 适配）。
//
// 挂载：/debug/pprof/{,cmdline,profile,symbol,trace,heap,goroutine,...}。
// 核心是把标准库 http.HandlerFunc 处理器转接到 framework 处理器（stdbridge）。
//
// 安全提醒：生产环境务必放在受保护路径（监听回环或挂 basicauth/jwt）。
package pprof

import (
	"io"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Mount 把剖析路由挂到 framework 服务。
func Mount(srv *framework.Server) {
	srv.GET("/debug/pprof/", index)
	srv.Any("/debug/pprof/cmdline", stdH(http.HandlerFunc(pprof.Cmdline)))
	srv.Any("/debug/pprof/symbol", stdH(http.HandlerFunc(pprof.Symbol)))
	srv.Any("/debug/pprof/:name", named)
}

func index(c *framework.Context) {
	// 手工总览（标准库 Index 依赖具体 profile 列表跳转，直接列我们挂的端点）
	var b strings.Builder
	b.WriteString("<html><head><title>/debug/pprof/</title></head><body>\n")
	b.WriteString("<h1>/debug/pprof/</h1>\n<ul>\n")
	for _, li := range []string{
		`<li><a href="profile?seconds=30">profile</a> — CPU profile（?seconds= 控制时长）</li>`,
		`<li><a href="trace?seconds=10">trace</a> — 执行跟踪</li>`,
		`<li><a href="heap?debug=1">heap</a> — 堆采样</li>`,
		`<li><a href="goroutine?debug=2">goroutine</a> — goroutine 栈</li>`,
		`<li><a href="block?debug=1">block</a> — 阻塞事件</li>`,
		`<li><a href="mutex?debug=1">mutex</a> — 锁竞争</li>`,
		`<li><a href="allocs?debug=1">allocs</a> — 分配采样</li>`,
		`<li><a href="threadcreate?debug=1">threadcreate</a> — 线程创建</li>`,
		`<li><a href="cmdline">cmdline</a></li>`,
		`<li><a href="symbol">symbol</a></li>`,
	} {
		b.WriteString(li + "\n")
	}
	b.WriteString("</ul></body></html>\n")
	c.HTML(200, b.String())
}

// named 具名 profile 与 profile/trace。
func named(c *framework.Context) {
	name := c.Param("name")
	switch name {
	case "profile":
		stdH(http.HandlerFunc(pprof.Profile))(c)
	case "trace":
		stdH(http.HandlerFunc(pprof.Trace))(c)
	default:
		serveNamed(c, name)
	}
}

// serveNamed heap/goroutine/allocs/block/mutex/threadcreate/...（含 ?debug=）。
func serveNamed(c *framework.Context, name string) {
	debug, _ := strconv.Atoi(c.Query("debug"))
	data, err := writeProfileTo(name, debug)
	if err != nil {
		c.Error(404, "profile not found: "+name)
		return
	}
	if debug > 0 {
		c.SetRespHeader("Content-Type", "text/plain; charset=utf-8")
	} else {
		c.SetRespHeader("Content-Type", "application/octet-stream")
		c.SetRespHeader("Content-Disposition", `attachment; filename="`+name+`-profile"`)
	}
	c.Status(200)
	_, _ = c.Write(data)
}

func seconds(c *framework.Context) int {
	v := c.Query("seconds")
	if v == "" {
		return 30
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 30
	}
	if n > 300 {
		n = 300
	}
	return n
}

// stdH 把标准库 http.Handler 包成 framework 处理器。
func stdH(h http.Handler) framework.HandlerFunc {
	return func(c *framework.Context) {
		h.ServeHTTP(newStdW(c), newStdReq(c))
	}
}

// newStdReq 由 framework 请求构造 *http.Request。
func newStdReq(c *framework.Context) *http.Request {
	r := &http.Request{
		Method:     c.Request.Method,
		URL:        parseURLFor(c),
		Header:     c.Request.Header,
		Host:       c.Request.Host,
		RemoteAddr: c.Request.RemoteAddr,
	}
	if c.Request.Body != nil {
		r.Body = io.NopCloser(c.Request.Body)
		r.ContentLength = c.Request.ContentLength
	}
	return r
}

// newStdW 把 framework 上下文包装为 http.ResponseWriter。
type stdW struct{ c *framework.Context }

func newStdW(c *framework.Context) *stdW    { return &stdW{c: c} }
func (w *stdW) Header() http.Header         { return w.c.RespHeader() }
func (w *stdW) WriteHeader(code int)        { w.c.Status(code) }
func (w *stdW) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w *stdW) Flush()                      { w.c.Flush() }

var _ http.Flusher = (*stdW)(nil)

func parseURLFor(c *framework.Context) *urlType {
	return parseURL(c.Request.Path, c.Request.RawQuery)
}
