package exttest

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ext/autotls"
	"github.com/ezscloud/searchboard/framework/ext/loadbalance"
	"github.com/ezscloud/searchboard/framework/ext/pprof"
	"github.com/ezscloud/searchboard/framework/ext/registry"
	"github.com/ezscloud/searchboard/framework/ext/reverseproxy"
)

// ---------------------------------------------------------------- reverseproxy

func TestReverseProxy(t *testing.T) {
	// 上游
	upBase := Start(t, func(s *framework.Server) {
		s.GET("/api/x", func(c *framework.Context) {
			c.JSON(200, map[string]any{"up": true, "xff": c.Header("X-Forwarded-For"), "host": c.Header("Host")})
		})
		s.POST("/api/echo", func(c *framework.Context) {
			b, _ := c.ReadBody(1 << 20)
			c.Data(200, "text/plain", b)
		})
	})
	// 剥 host:port
	upstream := strings.TrimPrefix(upBase, "http://")

	// 代理层
	proxyBase := Start(t, func(s *framework.Server) {
		s.Any("/api/*rest", reverseproxy.Handler(reverseproxy.Config{
			Targets: []loadbalance.Endpoint{{Addr: upstream}},
		}))
	})

	r, err := http.Get(proxyBase + "/api/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), `"up":true`) {
		t.Fatalf("proxy get: %d %s", r.StatusCode, b)
	}
	if !strings.Contains(string(b), "xff") || !strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("X-Forwarded-For missing: %s", b)
	}

	// POST 体透传
	resp, err := http.Post(proxyBase+"/api/echo", "text/plain", strings.NewReader("payload-42"))
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(pb) != "payload-42" {
		t.Fatalf("proxy post echo = %q", pb)
	}
}

// ---------------------------------------------------------------- pprof

func TestPprofEndpoints(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		pprof.Mount(s)
	})
	cli := StdClient()

	r, _ := cli.Get(base + "/debug/pprof/")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "goroutine") {
		t.Fatalf("pprof index: %d %s", r.StatusCode, b[:min(100, len(b))])
	}

	r2, _ := cli.Get(base + "/debug/pprof/goroutine?debug=1")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || !strings.Contains(string(b2), "goroutine") {
		t.Fatalf("goroutine profile: %d", r2.StatusCode)
	}

	r3, _ := cli.Get(base + "/debug/pprof/heap?debug=1")
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("heap profile: %d", r3.StatusCode)
	}

	r4, _ := cli.Get(base + "/debug/pprof/unknown-profile")
	r4.Body.Close()
	if r4.StatusCode != 404 {
		t.Fatalf("unknown profile: %d want 404", r4.StatusCode)
	}
}

// ---------------------------------------------------------------- registry

func TestRegistryMemory(t *testing.T) {
	m := registry.NewMemory(200 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	info := registry.ServiceInfo{Service: "svc", Addr: "10.0.0.1:9000", Metadata: map[string]string{"zone": "a"}}
	if err := m.Register(ctx, info); err != nil {
		t.Fatal(err)
	}
	eps, err := m.Resolve(context.Background(), "svc")
	if err != nil || len(eps) != 1 || eps[0].Addr != "10.0.0.1:9000" {
		t.Fatalf("resolve: %v %v", eps, err)
	}

	// Watch 变更信号
	w := m.Watch("svc")
	if err := m.Register(ctx, registry.ServiceInfo{Service: "svc", Addr: "10.0.0.2:9000"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w:
	case <-time.After(time.Second):
		t.Fatalf("watcher not notified")
	}

	// 注销
	if err := m.Deregister(ctx, info); err != nil {
		t.Fatal(err)
	}
	eps2, _ := m.Resolve(context.Background(), "svc")
	if len(eps2) != 1 || eps2[0].Addr != "10.0.0.2:9000" {
		t.Fatalf("after deregister: %v", eps2)
	}

	// TTL 剔除（不心跳）
	time.Sleep(300 * time.Millisecond)
	_, err = m.Resolve(context.Background(), "svc")
	if err != registry.ErrServiceNotFound {
		t.Fatalf("ttl eviction: %v", err)
	}
}

// ---------------------------------------------------------------- autotls

func TestAutotlsSelfIssue(t *testing.T) {
	dir := t.TempDir()
	m := autotls.New(dir, "127.0.0.1", "localhost").WithMode(autotls.ModeSelfSigned)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	<-m.Ready()

	tlsCfg := m.TLSConfig()
	if tlsCfg.GetCertificate == nil {
		t.Fatalf("no dynamic cert supply")
	}
	cert, err := tlsCfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get cert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatalf("empty chain")
	}

	// 二次 Run（磁盘加载路径）
	m2 := autotls.New(dir, "127.0.0.1").WithMode(autotls.ModeSelfSigned)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := m2.Run(ctx2); err != nil {
		t.Fatalf("reload: %v", err)
	}
	<-m2.Ready()
	c2, err := m2.TLSConfig().GetCertificate(nil)
	if err != nil || len(c2.Certificate) == 0 {
		t.Fatalf("reload cert: %v", err)
	}
}

// TestAutotlsACMEMode ACME 模式装配校验（不触网）：TLSConfig 动态供给 +
// 挑战处理器 404 直通（未挂 80 时行为）。
func TestAutotlsACMEMode(t *testing.T) {
	dir := t.TempDir()
	m := autotls.New(dir, "example.com").WithEmail("ops@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()
	select {
	case <-m.Ready():
	case <-time.After(2 * time.Second):
		t.Fatalf("acme manager not ready")
	}
	if m.Mode() != autotls.ModeACME {
		t.Fatalf("mode = %v", m.Mode())
	}
	tlsCfg := m.TLSConfig()
	if tlsCfg.GetCertificate == nil {
		t.Fatalf("acme tls config missing GetCertificate")
	}
	// 挑战处理器：未装配 inner 时 404 直通（此处已装配 → 透传 HTTPHandler）
	// 不实际发起挑战请求，仅验证处理器可构造。
	h := m.HTTPChallengeHandler()
	if h == nil {
		t.Fatalf("challenge handler nil")
	}
}
