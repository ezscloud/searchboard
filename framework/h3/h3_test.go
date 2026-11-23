package h3

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// TestH3RoundTrip：QUIC/HTTP3 全链路（自签证书）。
// 同套路由通过 StdHandler 同时服务 HTTP/3，验证 H3 请求 → framework 处理器 → H3 响应。
func TestH3RoundTrip(t *testing.T) {
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", IdleTimeout: 300 * time.Millisecond})
	srv.GET("/hello/:name", func(c *framework.Context) {
		c.JSON(200, map[string]any{"hello": c.Param("name"), "proto": c.Request.Proto})
	})
	srv.POST("/echo", func(c *framework.Context) {
		var in map[string]any
		if err := c.BindJSON(&in); err != nil {
			c.Error(400, err.Error())
			return
		}
		c.JSON(200, in)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start http: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	cert, err := GenerateSelfSignedCert([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	h3Addr := "127.0.0.1:18443"
	h3srv := New(Config{
		Addr:      h3Addr,
		TLSConfig: tlsConfOf(cert),
	}, srv.StdHandler())
	hctx, hcancel := context.WithCancel(context.Background())
	defer hcancel()
	if err := h3srv.Start(hctx); err != nil {
		t.Fatalf("start h3: %v", err)
	}
	t.Cleanup(func() {
		hcancel()
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		_ = h3srv.Shutdown(sctx)
	})

	cli := NewClient(nil) // 测试：自签证书，InsecureSkipVerify
	defer cli.Close()

	deadline := time.Now().Add(10 * time.Second)
	var status int
	var body []byte
	for {
		status, body, _, err = cli.Get(hctx, "https://"+h3Addr+"/hello/webfw")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("h3 get: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status != 200 {
		t.Fatalf("h3 status = %d", status)
	}
	if !strings.Contains(string(body), `"hello":"webfw"`) && !strings.Contains(string(body), `"hello": "webfw"`) {
		t.Fatalf("h3 body = %s", body)
	}
	if !strings.Contains(string(body), "HTTP/3") {
		t.Fatalf("h3 request proto not propagated: %s", body)
	}

	// POST + JSON 绑定（stdSink 链路）
	req, err := http.NewRequestWithContext(hctx, http.MethodPost,
		"https://"+h3Addr+"/echo", bytes.NewReader([]byte(`{"x":42}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.RoundTrip(req)
	if err != nil {
		t.Fatalf("h3 post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("h3 post status = %d", resp.StatusCode)
	}
	buf := new(strings.Builder)
	if _, err := copyBody(buf, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(buf.String(), "42") {
		t.Fatalf("h3 echo body = %s", buf.String())
	}
}

// tlsConfOf 组装 TLS 配置。
func tlsConfOf(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

func copyBody(w io.Writer, r io.Reader) (int64, error) { return io.Copy(w, r) }
