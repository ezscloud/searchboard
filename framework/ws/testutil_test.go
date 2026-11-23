package ws

import (
	"context"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// frameworkContext 测试中的上下文别名。
type frameworkContext = framework.Context

// testServer 测试用 HTTP 服务（路由由用例提供）。
type testServer struct {
	addr string
	srv  *framework.Server
}

func newTestServer(t *testing.T, handler framework.HandlerFunc, path string) *testServer {
	t.Helper()
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", IdleTimeout: 300 * time.Millisecond})
	srv.GET(path, handler)
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	return &testServer{addr: srv.Addr().String(), srv: srv}
}
