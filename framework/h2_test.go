package framework

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// h2 测试：自签证书 + std 客户端（ForceAttemptHTTP2 走 TLS-ALPN）与
// http2.Transport（AllowHTTP 走 h2c 明文）。

func newH2TestServer(t *testing.T, enableH2C bool) *Server {
	t.Helper()
	cert, err := genTestCert("127.0.0.1")
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	srv := New(Config{
		Addr:        "127.0.0.1:0",
		TLS:         &tls.Config{Certificates: []tls.Certificate{cert}},
		EnableH2C:   enableH2C,
		IdleTimeout: 300 * time.Millisecond,
	})
	srv.GET("/hello/:name", func(c *Context) {
		c.JSON(200, map[string]any{"hello": c.Param("name"), "proto": c.Request.Proto})
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	return srv
}

func genTestCert(host string) (tls.Certificate, error) {
	// 复用 h3 包思路但避免 import 环：直接内置一份极简自签
	return selfSignedForTest(host)
}

func TestHTTP2OverTLSALPN(t *testing.T) {
	srv := newH2TestServer(t, false)
	base := "https://" + srv.Addr().String()

	cli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 测试
			ForceAttemptHTTP2: true,
		},
		Timeout: 10 * time.Second,
	}

	resp, err := cli.Get(base + "/hello/h2")
	if err != nil {
		t.Fatalf("h2 get: %v", err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("proto = %s, want HTTP/2.0", resp.Proto)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"hello":"h2"`) && !strings.Contains(string(b), `"hello": "h2"`) {
		t.Fatalf("body = %s", b)
	}

	// 同客户端连发（h2 多路复用单连接）
	for i := 0; i < 5; i++ {
		r, err := cli.Get(base + "/hello/mux")
		if err != nil {
			t.Fatalf("h2 mux get#%d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
}

func TestH1StillWorksOverTLS(t *testing.T) {
	srv := newH2TestServer(t, false)
	cli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 测试
			ForceAttemptHTTP2: false,                                 // 不协商 h2
		},
		Timeout: 10 * time.Second,
	}
	resp, err := cli.Get("https://" + srv.Addr().String() + "/hello/h1")
	if err != nil {
		t.Fatalf("h1 get: %v", err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("proto = %s, want HTTP/1.1", resp.Proto)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestH2CPriorKnowledge(t *testing.T) {
	// 纯明文服务器（h2c 只在明文端口有意义；TLS 端口走 ALPN 分支）
	srv := New(Config{
		Addr:        "127.0.0.1:0",
		EnableH2C:   true,
		IdleTimeout: 300 * time.Millisecond,
	})
	srv.GET("/hello/:name", func(c *Context) {
		c.JSON(200, map[string]any{"hello": c.Param("name"), "proto": c.Request.Proto})
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	cli := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true, // 明文 h2c
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				return dialPlain(ctx, network, addr)
			},
		},
		Timeout: 10 * time.Second,
	}
	resp, err := cli.Get("http://" + srv.Addr().String() + "/hello/h2c")
	if err != nil {
		t.Fatalf("h2c get: %v", err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("h2c proto = %s, want HTTP/2.0", resp.Proto)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"proto":"HTTP/2.0"`) && !strings.Contains(string(b), `"proto": "HTTP/2.0"`) {
		t.Fatalf("h2c body proto mismatch: %s", b)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestH2CDisabledRejects(t *testing.T) {
	srv := newH2TestServer(t, false) // EnableH2C=false
	// 明文发 h2 前导：应作为坏请求被拒绝/断连，而不是进入 h2
	conn, err := dialPlainTimeout(srv.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte(clientPreface))
	if err != nil {
		t.Fatalf("write preface: %v", err)
	}
	buf := make([]byte, 256)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, rerr := conn.Read(buf)
	// 服务端应回 400/414/505 等错误响应或直接断连——两者都算正确行为
	if rerr != nil && n == 0 {
		return // 断连
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, "400") && !strings.Contains(resp, "505") && !strings.Contains(resp, "414") {
		t.Fatalf("h2c disabled should reject preface, got: %q", resp)
	}
}

// TestH2ShutdownDrainsInflight Shutdown 时正在飞的 h2 流应完成（GOAWAY
// 排空语义：现流允许继续，而非强关导致失败）。
func TestH2ShutdownDrainsInflight(t *testing.T) {
	srv := New(Config{
		Addr:        "127.0.0.1:0",
		EnableH2C:   true,
		IdleTimeout: 400 * time.Millisecond,
	})
	srv.GET("/slow", func(c *Context) {
		time.Sleep(300 * time.Millisecond)
		c.String(200, "slow-done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	defer func() { cancel(); _ = srv.Shutdown(context.Background()) }()

	cli := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				return dialPlain(ctx, network, addr)
			},
		},
		Timeout: 5 * time.Second,
	}
	type result struct {
		body string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := cli.Get("http://" + srv.Addr().String() + "/slow")
		if err != nil {
			ch <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ch <- result{body: string(b)}
	}()

	time.Sleep(80 * time.Millisecond) // 确保请求已在 h2 连接上在飞
	shutdownErr := srv.Shutdown(context.Background())

	r := <-ch
	if r.err != nil {
		t.Fatalf("inflight request killed by shutdown: %v", r.err)
	}
	if r.body != "slow-done" {
		t.Fatalf("inflight body = %q", r.body)
	}
	if shutdownErr != nil {
		t.Fatalf("shutdown err: %v", shutdownErr)
	}
}

// TestH2ShutdownSendsGoAway 用手工 framer 客户端直读服务端 Shutdown 发出的
// GOAWAY 帧（NO_ERROR），确认 goawayH2Conns 手工帧路径生效。
//
// 区分度：IdleTimeout 设为 30s（排除 x/net/http2 空闲自 GOAWAY 干扰），
// Shutdown 的 ctx 设 2s（超时前必须读到手工 GOAWAY，否则强关断连失败）。
func TestH2ShutdownSendsGoAway(t *testing.T) {
	srv := New(Config{
		Addr:        "127.0.0.1:0",
		EnableH2C:   true,
		IdleTimeout: 30 * time.Second,
	})
	ctx, ctxCancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		ctxCancel()
		t.Fatalf("start: %v", err)
	}
	defer func() { ctxCancel(); _ = srv.Shutdown(context.Background()) }()

	// 手工 h2c 客户端：前导 + 空 SETTINGS
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(clientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("settings: %v", err)
	}

	// 读取并跳过服务端初始帧（SETTINGS 等）直到出现 GOAWAY：
	// Shutdown 前 x/net/http2 只会回 SETTINGS/SETTINGS ACK，不发 GOAWAY；
	// GOAWAY 只能来自我们的 goawayH2Conns（idle 30s 排除内部 GOAWAY）。
	go func() {
		time.Sleep(150 * time.Millisecond)
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var goaway *http2.GoAwayFrame
	for i := 0; i < 16; i++ {
		f, err := fr.ReadFrame()
		if err != nil {
			if goaway != nil {
				break // 连接随后被关闭属预期
			}
			t.Fatalf("read frame before GOAWAY: %v", err)
		}
		if g, ok := f.(*http2.GoAwayFrame); ok {
			goaway = g
			break
		}
	}
	if goaway == nil {
		t.Fatal("no GOAWAY frame received during shutdown")
	}
	if goaway.ErrCode != http2.ErrCodeNo {
		t.Fatalf("GOAWAY errcode = %v, want NO_ERROR", goaway.ErrCode)
	}
	if goaway.LastStreamID != 0x7fffffff {
		t.Fatalf("GOAWAY lastStreamID = %#x, want 0x7fffffff", goaway.LastStreamID)
	}
}
