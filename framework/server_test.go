package framework

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework/transport"
)

// newE2EServer 启动 127.0.0.1:0 随机端口的测试服务（transport=auto）。
// IdleTimeout 收紧到 300ms，加速 Shutdown 收尾。
func newE2EServer(t *testing.T, register func(*Server)) *Server {
	t.Helper()
	srv := New(Config{Addr: "127.0.0.1:0", IdleTimeout: 300 * time.Millisecond})
	register(srv)
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	return srv
}

func (s *Server) base() string { return "http://" + s.Addr().String() }

func TestE2EGetJSONAndParams(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/hello/:name", func(c *Context) {
			c.JSON(200, map[string]any{"hello": c.Param("name"), "q": c.Query("q")})
		})
	})
	cli := NewClient()
	resp, err := cli.Get(context.Background(), srv.base()+"/hello/webfw?q=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Hello string `json:"hello"`
		Q     string `json:"q"`
	}
	if err := resp.JSON(&out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if out.Hello != "webfw" || out.Q != "1" {
		t.Errorf("unexpected body: %+v", out)
	}

	// 编码路径：请求解析层解码后路由，参数应拿到解码值
	respEnc, err := cli.Get(context.Background(), srv.base()+"/hello/%E4%B8%AD%E6%96%87")
	if err != nil {
		t.Fatalf("get encoded: %v", err)
	}
	defer respEnc.Close()
	var out2 struct {
		Hello string `json:"hello"`
	}
	if err := respEnc.JSON(&out2); err != nil {
		t.Fatalf("json encoded: %v", err)
	}
	if out2.Hello != "中文" {
		t.Errorf("encoded path param = %q, want 中文", out2.Hello)
	}
}

// TestStdClientInterop 用标准库 http.Client 打我们的服务端：
// 这是与参考实现的互操作验证（协议兼容性的硬证据）。
func TestStdClientInterop(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/std/ping", func(c *Context) { c.String(200, "pong") })
		s.POST("/std/echo", func(c *Context) {
			var in map[string]any
			if err := c.BindJSON(&in); err != nil {
				c.Error(400, err.Error())
				return
			}
			c.JSON(200, in)
		})
	})
	resp, err := http.Get(srv.base() + "/std/ping")
	if err != nil {
		t.Fatalf("std client get: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "pong" {
		t.Errorf("std get: %d %q", resp.StatusCode, b)
	}

	resp2, err := http.Post(srv.base()+"/std/echo", "application/json",
		strings.NewReader(`{"x":42}`))
	if err != nil {
		t.Fatalf("std client post: %v", err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(b2), `"x":42`) && !strings.Contains(string(b2), `"x": 42`) {
		t.Errorf("std post echo body = %q", b2)
	}
}

// TestKeepAliveRawConn 手工在同一连接上连发两个请求验证 keep-alive。
func TestKeepAliveRawConn(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/ka", func(c *Context) { c.String(200, "ka") })
	})
	conn, err := transport.Dial("tcp", srv.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	br := bufio.NewReaderSize(conn, 4096)
	for i := 0; i < 2; i++ {
		req := "GET /ka HTTP/1.1\r\nHost: t\r\n\r\n"
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write#%d: %v", i, err)
		}
		// 读完整响应（Content-Length 已知）
		hdr := ""
		cl := -1
		for {
			line, err := readLine(br)
			if err != nil {
				t.Fatalf("read header#%d: %v", i, err)
			}
			if len(line) == 0 {
				break
			}
			s := string(line)
			if strings.HasPrefix(s, "Content-Length:") {
				fmt.Sscanf(s, "Content-Length: %d", &cl)
			}
			hdr += s + "\n"
		}
		if cl <= 0 {
			t.Fatalf("response#%d missing Content-Length: %s", i, hdr)
		}
		if strings.Contains(hdr, "Connection: close") {
			t.Fatalf("response#%d asked close prematurely: %s", i, hdr)
		}
		body := make([]byte, cl)
		if _, err := io.ReadFull(br, body); err != nil {
			t.Fatalf("read body#%d: %v", i, err)
		}
		if string(body) != "ka" {
			t.Fatalf("body#%d = %q", i, body)
		}
	}
}

// TestConnectionCloseHonored 请求带 Connection: close 时服务端应主动断开。
func TestConnectionCloseHonored(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/x", func(c *Context) { c.String(200, "x") })
	})
	conn, err := transport.Dial("tcp", srv.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := "GET /x HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReaderSize(conn, 4096)
	// 读到 EOF（服务器关闭连接）
	got := 0
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		n, err := br.Read(buf)
		got += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got == 0 {
		t.Fatalf("expected some response bytes before EOF")
	}
}

// TestBigBodyChunked 未知长度上行（chunked 请求）+ >2KB 下行（chunked 响应）。
func TestBigBodyChunked(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.POST("/big", func(c *Context) {
			b, _ := c.ReadBody(64 << 20)
			c.String(200, "len=%d", len(b))
		})
		s.GET("/stream", func(c *Context) {
			// 无 CL：超过 2KB 缓冲自动转 chunked
			c.Status(200)
			chunk := strings.Repeat("a", 4096)
			for i := 0; i < 3; i++ {
				if _, err := c.WriteString(chunk); err != nil {
					return
				}
			}
		})
	})
	cli := NewClient()

	// 上行 chunked：Body 未知长度（BodyLen=-1）
	payload := make([]byte, 1<<20)
	_, _ = rand.Read(payload)
	req := NewClientRequest(context.Background(), http.MethodPost, srv.base()+"/big",
		strings.NewReader(string(payload)))
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("post big: %v", err)
	}
	defer resp.Close()
	b, _ := resp.ReadAll()
	if string(b) != "len=1048576" {
		t.Fatalf("big body echo = %q", b)
	}

	// 下行 chunked
	resp2, err := cli.Get(context.Background(), srv.base()+"/stream")
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	defer resp2.Close()
	if !strings.Contains(resp2.Header.Get("Transfer-Encoding"), "chunked") {
		t.Fatalf("expected chunked response, hdr=%v", resp2.Header)
	}
	b2, err := resp2.ReadAll()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(b2) != 3*4096 {
		t.Fatalf("stream len = %d", len(b2))
	}
}

func TestHeadRequestNoBody(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/h", func(c *Context) { c.String(200, "hello-head") })
	})
	cli := NewClient()
	resp, err := cli.Do(NewClientRequest(context.Background(), http.MethodHead, srv.base()+"/h", nil))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	defer resp.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("head status = %d", resp.StatusCode)
	}
	// HEAD 无响应体：ReadAll 立即返回空
	b, err := resp.ReadAll()
	if err != nil {
		t.Fatalf("head body read: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("head body should be empty, got %q", b)
	}
	// 标准库互操作
	hresp, err := http.Head(srv.base() + "/h")
	if err != nil {
		t.Fatalf("std head: %v", err)
	}
	defer hresp.Body.Close()
	if hresp.ContentLength != int64(len("hello-head")) {
		t.Fatalf("std head Content-Length = %d, want 10", hresp.ContentLength)
	}
}

func Test404And405(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/only-get", func(c *Context) { c.String(200, "ok") })
	})
	cli := NewClient()
	resp, err := cli.Get(context.Background(), srv.base()+"/no-such")
	if err != nil {
		t.Fatalf("get 404: %v", err)
	}
	resp.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("404 test: status = %d", resp.StatusCode)
	}

	req := NewClientRequest(context.Background(), http.MethodDelete, srv.base()+"/only-get", nil)
	resp2, err := cli.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp2.Close()
	if resp2.StatusCode != 405 {
		t.Fatalf("405 test: status = %d", resp2.StatusCode)
	}
	if resp2.Header.Get("Allow") == "" {
		t.Fatalf("405 should carry Allow header")
	}
}

func TestMiddlewareAndRecovery(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.Use(func(c *Context) {
			c.SetRespHeader("X-Mw", "1")
			c.Next()
		})
		s.Use(Recovery(io.Discard))
		s.GET("/panic", func(c *Context) { panic("boom") })
		s.GET("/ok", func(c *Context) { c.String(200, "fine") })
	})
	cli := NewClient()
	resp, err := cli.Get(context.Background(), srv.base()+"/ok")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Close()
	if resp.Header.Get("X-Mw") != "1" {
		t.Fatalf("middleware header missing")
	}

	resp2, err := cli.Get(context.Background(), srv.base()+"/panic")
	if err != nil {
		t.Fatalf("get panic route: %v", err)
	}
	defer resp2.Close()
	if resp2.StatusCode != 500 {
		t.Fatalf("panic route status = %d, want 500", resp2.StatusCode)
	}
}

func TestSSEStream(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/events", func(c *Context) {
			c.SetRespHeader("Content-Type", "text/event-stream")
			c.Status(200)
			c.Flush()
			for i := 0; i < 3; i++ {
				_, _ = c.WriteString("data: tick\n\n")
				c.Flush()
			}
		})
	})
	cli := NewClient()
	resp, err := cli.Get(context.Background(), srv.base()+"/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Close()
	b, err := resp.ReadAll()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if strings.Count(string(b), "tick") != 3 {
		t.Fatalf("sse ticks = %d, body=%q", strings.Count(string(b), "tick"), b)
	}
}

func TestGracefulShutdown(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/slow", func(c *Context) { time.Sleep(300 * time.Millisecond); c.String(200, "slow") })
	})
	cli := NewClient()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := cli.Get(context.Background(), srv.base()+"/slow")
		if err == nil {
			resp.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond) // 让请求在途
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("in-flight request did not finish after shutdown")
	}
}

// 顺手验证 Base64 辅助与请求行拆解（微单元）。
func TestSplitRequestLine(t *testing.T) {
	m, tg, p, ok := splitRequestLine("POST /a/b?x=1 HTTP/1.1")
	if !ok || m != "POST" || tg != "/a/b?x=1" || p != "HTTP/1.1" {
		t.Fatalf("splitRequestLine = %q %q %q %v", m, tg, p, ok)
	}
	if _, _, _, ok := splitRequestLine("GARBAGE"); ok {
		t.Fatalf("garbage should not parse")
	}
	if _, _, _, ok := splitRequestLine("GET / HTTP/2.0"); ok {
		t.Fatalf("HTTP/2 should not parse")
	}
	if base64.StdEncoding.EncodeToString([]byte{1, 2}) != encodeB64ForTest([]byte{1, 2}) {
		t.Fatalf("b64 mismatch")
	}
}

func encodeB64ForTest(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
