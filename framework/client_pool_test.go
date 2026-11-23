package framework

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errFakeEOF = errors.New("fake conn: eof")

// fakeConn 最小 net.Conn 实现（清扫测试用）。
type fakeConn struct{ closed atomic.Bool }

func (c *fakeConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *fakeConn) Close() error                       { c.closed.Store(true); return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return nil }
func (c *fakeConn) RemoteAddr() net.Addr               { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

func newFakeConn() *fakeConn { return &fakeConn{} }

// TestClientConnPoolReuse 池化客户端连续请求：连接应被复用（服务端连接计数 = 1）。
func TestClientConnPoolReuse(t *testing.T) {
	var conns int32
	srv := newE2EServer(t, func(s *Server) {
		s.Use(func(c *Context) { c.Next() })
		s.GET("/ping", func(c *Context) { c.String(200, "pong") })
	})
	// 用自定义 hook 计连接数：起服务前不好注入；改由响应可复用性间接验证
	_ = conns

	cli := NewClient()
	defer cli.CloseIdleConnections()

	for i := 0; i < 10; i++ {
		resp, err := cli.Get(context.Background(), srv.base()+"/ping")
		if err != nil {
			t.Fatalf("get#%d: %v", i, err)
		}
		if _, err := resp.ReadAll(); err != nil {
			t.Fatalf("read#%d: %v", i, err)
		}
		if err := resp.Close(); err != nil {
			t.Fatalf("close#%d: %v", i, err)
		}
	}
	// 池中应有空闲连接（复用成功）
	p := cli.poolOf()
	if p == nil {
		t.Fatalf("pool not initialized")
	}
	p.mu.Lock()
	idle := 0
	for _, list := range p.idle {
		idle += len(list)
	}
	p.mu.Unlock()
	if idle != 1 {
		t.Fatalf("expected exactly 1 idle pooled conn, got %d", idle)
	}
}

// TestClientPoolConcurrent 并发池化：每 host 上限内的复用与拨号并存。
func TestClientPoolConcurrent(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/c", func(c *Context) { c.String(200, "ok") })
	})
	cli := NewClient()
	cli.MaxIdleConnsPerHost = 2
	defer cli.CloseIdleConnections()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := cli.Get(context.Background(), srv.base()+"/c")
			if err != nil {
				errCh <- err
				return
			}
			if _, err := resp.ReadAll(); err != nil {
				errCh <- err
				return
			}
			resp.Close()
		}()
	}
	wg.Wait()
	select {
	case e := <-errCh:
		t.Fatalf("concurrent pooled: %v", e)
	default:
	}
}

// TestClientPoolHeadReuse HEAD 无体：读尽即归还复用。
func TestClientPoolHeadReuse(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/h", func(c *Context) { c.String(200, "body") })
	})
	cli := NewClient()
	defer cli.CloseIdleConnections()

	for i := 0; i < 3; i++ {
		resp, err := cli.Do(NewClientRequest(context.Background(), http.MethodHead, srv.base()+"/h", nil))
		if err != nil {
			t.Fatalf("head#%d: %v", i, err)
		}
		resp.Close()
	}
	p := cli.poolOf()
	p.mu.Lock()
	idle := 0
	for _, list := range p.idle {
		idle += len(list)
	}
	p.mu.Unlock()
	if idle != 1 {
		t.Fatalf("head reuse: idle=%d want 1", idle)
	}
}

// TestClientDisablePool 非池化路径仍工作（Connection: close）。
func TestClientDisablePool(t *testing.T) {
	srv := newE2EServer(t, func(s *Server) {
		s.GET("/x", func(c *Context) { c.String(200, "x") })
	})
	cli := NewClient()
	cli.DisablePool = true
	for i := 0; i < 3; i++ {
		resp, err := cli.Get(context.Background(), srv.base()+"/x")
		if err != nil {
			t.Fatalf("nopool#%d: %v", i, err)
		}
		if _, err := resp.ReadAll(); err != nil {
			t.Fatalf("read#%d: %v", i, err)
		}
		resp.Close()
	}
	if cli.poolOf() != nil {
		t.Fatalf("pool should be nil when disabled")
	}
}

// TestPoolSweepExpiry 空闲超时清扫：TTL 过期连接被关闭。
func TestPoolSweepExpiry(t *testing.T) {
	p := newConnPool(2, 50*time.Millisecond)
	pc := &pooledConn{conn: newFakeConn(), key: "k"}
	p.release(pc, true)
	time.Sleep(180 * time.Millisecond) // 3 个清扫周期
	p.mu.Lock()
	idle := len(p.idle["k"])
	p.mu.Unlock()
	if idle != 0 {
		t.Fatalf("expired conn should be swept, idle=%d", idle)
	}
	p.ClosePool()
}

// fakeConn 已移至 import 区下方（见上）。
var _ net.Conn = (*fakeConn)(nil)
