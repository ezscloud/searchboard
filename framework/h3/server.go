// Package h3：QUIC / HTTP-3 支持（基于 quic-go v0.65 + 其 http3 包）。
//
// 复用方式：framework.Server 的同套路由通过 StdHandler() 转成标准
// http.Handler 后挂到本包 Server——一套路由同时跑 HTTP/1.1（netpoll/std
// 传输层）与 HTTP/3（QUIC over UDP）。
//
// TLS 为 QUIC 前置条件：CertFile/KeyFile 加载或直接给含证书的 TLSConfig；
// 开发/测试可用 GenerateSelfSignedCert 现签。
package h3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	h3lib "github.com/quic-go/quic-go/http3"
)

// Server HTTP/3 服务端。
type Server struct {
	cfg Config

	inner     *h3lib.Server
	handler   http.Handler
	started   atomic.Bool
	serveDone chan error
}

// Config HTTP/3 服务配置。
type Config struct {
	// Addr UDP 监听地址，如 "127.0.0.1:8443" / ":8443"。
	Addr string

	// TLSConfig 必须含 Certificates（或改用 CertFile/KeyFile）。
	TLSConfig *tls.Config

	// CertFile / KeyFile PEM 证书对（设置则覆盖 TLSConfig 的证书来源）。
	CertFile string
	KeyFile  string

	// IdleTimeout 空闲连接关闭时限（0=默认行为）。
	IdleTimeout time.Duration

	// Logf 事件日志。
	Logf func(format string, args ...any)
}

// New 创建 HTTP/3 服务端；handler 通常来自 framework.Server.StdHandler()。
func New(cfg Config, handler http.Handler) *Server {
	return &Server{cfg: cfg, handler: handler}
}

// buildTLS 校验/组装 TLS 配置。
func (c *Config) buildTLS() (*tls.Config, error) {
	if c.CertFile != "" && c.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("h3: load cert: %w", err)
		}
		base := c.TLSConfig
		if base == nil {
			base = &tls.Config{}
		}
		out := base.Clone()
		out.Certificates = append(out.Certificates, cert)
		return out, nil
	}
	if c.TLSConfig == nil || len(c.TLSConfig.Certificates) == 0 && c.TLSConfig.GetCertificate == nil {
		return nil, errors.New("h3: QUIC requires TLS certificate (set CertFile/KeyFile or TLSConfig)")
	}
	return c.TLSConfig, nil
}

// Start 创建监听并启动后台服务（非阻塞；返回后立即可服务）。
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.started.Load() {
		return errors.New("h3: server already started")
	}
	tlsConf, err := s.cfg.buildTLS()
	if err != nil {
		return err
	}
	inner := &h3lib.Server{
		Addr:        s.cfg.Addr,
		TLSConfig:   tlsConf,
		Handler:     s.handler,
		IdleTimeout: s.cfg.IdleTimeout,
	}
	s.inner = inner
	s.started.Store(true)
	s.serveDone = make(chan error, 1)
	go func() {
		if s.cfg.Logf != nil {
			s.cfg.Logf("h3 serving on %s (QUIC/HTTP3)", s.cfg.Addr)
		}
		s.serveDone <- inner.ListenAndServe()
	}()
	return nil
}

// ListenAndServe 阻塞服务（UDP）。
func (s *Server) ListenAndServe() error {
	if err := s.Start(context.Background()); err != nil {
		return err
	}
	return <-s.serveDone
}

// Run 服务直至 ctx 取消（随后 ≤3s 优雅停机）。
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	select {
	case err := <-s.serveDone:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	}
}

// Shutdown 优雅停机（等待活跃连接 ≤ctx 时限）。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.inner == nil {
		return nil
	}
	return s.inner.Shutdown(ctx)
}

// Close 立即关闭。
func (s *Server) Close() error {
	if s.inner == nil {
		return nil
	}
	return s.inner.Close()
}
