// Package exttest 为 ext 扩展包提供共享 e2e 工具。
package exttest

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// Start 起真实 framework 服务（127.0.0.1:0）并注册路由，返回 base URL（自动清理）。
func Start(t *testing.T, register func(s *framework.Server)) string {
	t.Helper()
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", IdleTimeout: 300 * time.Millisecond})
	register(srv)
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	return "http://" + srv.Addr().String()
}

// StdClient 标准库客户端（5s 超时 + cookie jar）。
func StdClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: 5 * time.Second, Jar: jar}
}
