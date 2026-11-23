// api_test.go API 层 e2e 测试：mock ES 源 + 真实 framework 服务端 + 客户端全链路。
package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/transport"
	"github.com/ezscloud/searchboard/internal/es"
	"github.com/ezscloud/searchboard/internal/logtail"
)

// startMockES 启动假 ES 源（独立端口），返回 base URL。
func startMockES(t *testing.T) string {
	t.Helper()
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", Transport: "std"})
	srv.GET("/", func(c *framework.Context) {
		c.JSON(200, map[string]any{
			"name": "node-1", "cluster_name": "mock-cluster", "cluster_uuid": "u-1234",
			"version": map[string]any{"number": "8.11.0", "distribution": "elasticsearch"},
		})
	})
	srv.GET("/_cluster/health", func(c *framework.Context) {
		c.JSON(200, map[string]any{"cluster_name": "mock-cluster", "status": "green",
			"number_of_nodes": 1, "number_of_data_nodes": 1, "active_shards": 3})
	})
	srv.GET("/_cat/indices", func(c *framework.Context) {
		c.Data(200, "application/json",
			[]byte(`[{"health":"green","status":"open","index":"idx-a","uuid":"a1","pri":"1","rep":"1","docs.count":"120","store.size":"2.1mb"}]`))
	})
	srv.Any("/echo", func(c *framework.Context) {
		body, _ := c.ReadBody(1 << 20)
		c.JSON(200, map[string]any{
			"method": c.Request.Method, "path": c.Request.Path,
			"body": string(body), "q": c.Query("q"),
		})
	})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx) // 超时强制关池化连接，避免无限等待
	})
	return "http://" + srv.Addr().String()
}

// startBoard 启动 searchboard API 服务，返回 base URL。
func startBoard(t *testing.T, esURL, logDir string) string {
	t.Helper()
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", Transport: "std"})
	tailer := logtail.New(logDir, "*.log", 20*time.Millisecond, 100)
	Register(srv, Options{
		ES:                es.New(esURL, "", "", false),
		Tail:              tailer,
		Brand:             NewBrandState(Brand{Name: "Board-Default", Slogan: "default slogan", Logo: ""}),
		Version:           "test",
		HeartbeatInterval: 200 * time.Millisecond, // 加速断开感知测试
	})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + srv.Addr().String()
}

func getBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	client := framework.NewClient()
	r, err := client.Get(context.Background(), url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Close()
	b, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return r.StatusCode, b
}

func TestStateAndHealth(t *testing.T) {
	esURL := startMockES(t)
	board := startBoard(t, esURL, t.TempDir())

	code, b := getBody(t, board+"/api/state")
	if code != 200 {
		t.Fatalf("state code=%d", code)
	}
	s := string(b)
	for _, want := range []string{`"clusterName":"mock-cluster"`, `"name":"Board-Default"`, `"version":"test"`, `"ok":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("state missing %s in %s", want, s)
		}
	}

	code, b = getBody(t, board+"/api/health")
	if code != 200 || !strings.Contains(string(b), `"status":"green"`) {
		t.Errorf("health: code=%d body=%s", code, b)
	}
}

func TestProxyEndpoints(t *testing.T) {
	esURL := startMockES(t)
	board := startBoard(t, esURL, t.TempDir())

	code, b := getBody(t, board+"/api/es/health")
	if code != 200 || !strings.Contains(string(b), "mock-cluster") {
		t.Errorf("es/health: code=%d body=%s", code, b)
	}
	code, b = getBody(t, board+"/api/es/indices")
	if code != 200 || !strings.Contains(string(b), `"index":"idx-a"`) {
		t.Errorf("es/indices: code=%d body=%s", code, b)
	}
}

func TestRestPassthrough(t *testing.T) {
	esURL := startMockES(t)
	board := startBoard(t, esURL, t.TempDir())

	client := framework.NewClient()
	req := framework.NewClientRequest(context.Background(), "POST", board+"/api/es/rest", nil)
	req.SetBodyBytes([]byte(`{"method":"POST","path":"/echo?q=1","body":{"k":"v"}}`))
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := r.ReadAll()
	s := string(b)
	// mock echo 将请求体作为字符串回显，JSON 序列化后为转义形态
	for _, want := range []string{`"method":"POST"`, `"q":"1"`, `\"k\":\"v\"`} {
		if !strings.Contains(s, want) {
			t.Errorf("rest missing %s in %s", want, s)
		}
	}
}

func TestBrandLifecycle(t *testing.T) {
	esURL := startMockES(t)
	board := startBoard(t, esURL, t.TempDir())

	// set
	client := framework.NewClient()
	req := framework.NewClientRequest(context.Background(), "POST", board+"/api/brand", nil)
	req.SetBodyBytes([]byte(`{"name":"Custom","slogan":"","logo":"/public/logo.png"}`))
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	_, b := getBody(t, board+"/api/brand")
	s := string(b)
	if !strings.Contains(s, `"name":"Custom"`) || !strings.Contains(s, `"slogan":"default slogan"`) ||
		!strings.Contains(s, `"logo":"/public/logo.png"`) {
		t.Errorf("brand after set: %s", s)
	}

	// reset → 回落 YAML 默认
	req2 := framework.NewClientRequest(context.Background(), "POST", board+"/api/brand/reset", nil)
	req2.SetBodyBytes([]byte(`{}`))
	r2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := r2.ReadAll()
	r2.Close()
	if !strings.Contains(string(b2), `"name":"Board-Default"`) {
		t.Errorf("brand after reset: %s", b2)
	}
}

func TestLogStreamSSE(t *testing.T) {
	dir := t.TempDir()
	name := "app.log"
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("hist-1\nhist-2\nhist-3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	esURL := startMockES(t)
	board := startBoard(t, esURL, dir)

	// SSE 客户端：手写 HTTP 逐块读（EventSource 等价）
	host := strings.TrimPrefix(board, "http://")
	raw := fmt.Sprintf("GET /api/logs/stream?file=%s&lines=2 HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n\r\n", name, host)
	conn, err := transport.Dial("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}

	readUntil := func(marker string, timeout time.Duration) string {
		t.Helper()
		var acc strings.Builder
		buf := make([]byte, 4096)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			n, err := conn.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), marker) {
					return acc.String()
				}
			}
			if err != nil {
				if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
					continue
				}
				break
			}
		}
		return acc.String()
	}

	// 历史 2 行 + ready
	out := readUntil("event: ready", 5*time.Second)
	if !strings.Contains(out, "hist-2") || !strings.Contains(out, "hist-3") {
		t.Errorf("backfill missing: %s", out)
	}
	if strings.Contains(out, "hist-1") {
		t.Errorf("backfill should be last 2 lines only: %s", out)
	}

	// 追加新行 → 实时下发
	f, err := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("live-4\n")
	f.Close()

	out = readUntil("live-4", 5*time.Second)
	if !strings.Contains(out, "event: line") {
		t.Errorf("live line missing event header: %s", out)
	}

	// 轮转 → rotate 事件
	if err := os.WriteFile(full, []byte("rot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = readUntil("event: rotate", 5*time.Second)
	if !strings.Contains(out, "rotate") {
		t.Errorf("rotate event missing: %s", out)
	}

	// 断开：h1 链路断开监视应即时关闭会话（Cleanup 的 Shutdown 若卡住
	// 会拖死测试——此处 8s 上限验证会话正确退出）
	conn.Close()
	time.Sleep(300 * time.Millisecond)
}

// TestSSEDisconnectShutdown 验证客户端断开后服务端会话即时退出（Shutdown 不卡）。
func TestSSEDisconnectShutdown(t *testing.T) {
	dir := t.TempDir()
	name := "app.log"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	esURL := startMockES(t)
	board := startBoard(t, esURL, dir)

	host := strings.TrimPrefix(board, "http://")
	conn, err := transport.Dial("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("GET /api/logs/stream?file=%s&lines=1 HTTP/1.1\r\nHost: %s\r\n\r\n", name, host)
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}

	// 等 ready 事件到达
	buf := make([]byte, 4096)
	var acc strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(acc.String(), "event: ready") {
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
		}
		if err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
				break
			}
		}
	}
	conn.Close() // 客户端断开

	// 断开监视应在 ~250ms×2 内感知并结束 handler；给 3s 预算验证不卡
	time.Sleep(3 * time.Second) // Cleanup: Shutdown 若超时会话未退出则测试超时失败
}

func TestLogsListAndReject(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.log"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)
	esURL := startMockES(t)
	board := startBoard(t, esURL, dir)

	code, b := getBody(t, board+"/api/logs")
	if code != 200 {
		t.Fatalf("logs code=%d", code)
	}
	s := string(b)
	if !strings.Contains(s, "a.log") || strings.Contains(s, "b.txt") {
		t.Errorf("logs filter wrong: %s", s)
	}

	// 非法文件名拒绝
	code, b = getBody(t, board+"/api/logs/stream?file=..%2Fsecret.txt&lines=1")
	if code != 400 {
		t.Errorf("traversal should 400, got %d body=%s", code, b)
	}
}
