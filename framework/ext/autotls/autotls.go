// Package autotls Let's Encrypt 自动证书。
//
// 生产路径：基于 golang.org/x/crypto/acme/autocert（HTTP-01 挑战 +
// DirCache 持久化 + 到期自动续签）。HTTPChallengeHandler() 暴露
// /.well-known/acme-challenge/ 处理器，挂到 80 端口的 framework 服务即可
// 完成 HTTP-01 回源（Let's Encrypt 要求 80 可达）。
//
// 离线/开发回退：WithMode(ModeSelfSigned)（或 ACME 探测失败时）走本地
// 自签（SAN 覆盖全部 host），证书目录结构与 ACME 一致可平滑切换。
//
// 用法（生产）：
//
//	m := autotls.New("certs", "example.com").WithEmail("ops@example.com")
//	go m.Run(ctx)                          // 续签循环（autocert 内部计时器）
//	srv := framework.New(framework.Config{
//	    Addr: ":443",
//	    TLS:  m.TLSConfig(),                // 动态证书供给（含自动续签）
//	})
//	srv80 := framework.New(framework.Config{Addr: ":80"})
//	srv80.Any("/*p", m.HTTPChallengeHandler()) // ACME HTTP-01 回源
package autotls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/ezscloud/searchboard/framework"
)

// Mode 证书供给模式。
type Mode string

const (
	// ModeACME 生产：Let's Encrypt ACME（HTTP-01）。
	ModeACME Mode = "acme"

	// ModeSelfSigned 离线/开发：本地自签（90 天，SAN 覆盖全部 host）。
	ModeSelfSigned Mode = "self-signed"
)

// Manager 证书管理器。
type Manager struct {
	hosts []string
	dir   string
	email string
	mode  Mode

	inner *autocert.Manager // ACME 路径
	mu    sync.Mutex
	cert  *tls.Certificate // 自签路径
	ready chan struct{}
}

// New 创建管理器（默认 ACME 模式；网络不可达时可用 WithMode 切自签）。
// dir 为证书目录（自动创建；ACME 模式作 DirCache，自签模式落盘）。
func New(dir string, hosts ...string) *Manager {
	return &Manager{hosts: hosts, dir: dir, mode: ModeACME, ready: make(chan struct{})}
}

// WithEmail 注册邮箱（Let's Encrypt 到期提醒）。
func (m *Manager) WithEmail(email string) *Manager { m.email = email; return m }

// WithMode 强制模式（acme / self-signed）。
func (m *Manager) WithMode(mode Mode) *Manager { m.mode = mode; return m }

// Hosts 域名列表。
func (m *Manager) Hosts() []string { return append([]string(nil), m.hosts...) }

// Mode 当前模式。
func (m *Manager) Mode() Mode { return m.mode }

// TLSConfig 动态证书供给（GetCertificate 每次握手取最新证书；含续签）。
func (m *Manager) TLSConfig() *tls.Config {
	if m.mode == ModeACME {
		m.mu.Lock()
		inner := m.inner
		m.mu.Unlock()
		if inner != nil {
			return inner.TLSConfig()
		}
	}
	return &tls.Config{GetCertificate: m.getCertificate}
}

// Ready 证书就绪信号（Run 后置位：ACME 即刻可服务；自签为签发完成）。
func (m *Manager) Ready() <-chan struct{} { return m.ready }

// HTTPChallengeHandler ACME HTTP-01 挑战处理器（挂 80 端口）。
// 非 ACME 模式返回 404 直通。
func (m *Manager) HTTPChallengeHandler() framework.HandlerFunc {
	if m.mode == ModeACME {
		m.mu.Lock()
		inner := m.inner
		m.mu.Unlock()
		if inner != nil {
			return stdToFramework(inner.HTTPHandler(nil))
		}
	}
	return func(c *framework.Context) {
		c.Status(http.StatusNotFound)
		_, _ = c.WriteString("404 not found")
	}
}

// Run 启动供给循环（阻塞直至 ctx 取消）。
//
// ACME 模式：装配 autocert.Manager（Prompt/HostPolicy/DirCache/Email），
// 签发由首握手惰性触发（autocert 设计），续签由其内部计时器驱动；
// 本方法同时保持 ctx 生命周期。
// 自签模式：加载或签发并落盘，此后每 24h 检查，到期前 30 天续签。
func (m *Manager) Run(ctx context.Context) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}

	if m.mode == ModeACME {
		m.mu.Lock()
		if m.inner == nil {
			m.inner = &autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: hostPolicyOf(m.hosts),
				Cache:      autocert.DirCache(m.dir),
				Email:      m.email,
			}
		}
		m.mu.Unlock()
		close(m.ready)
		<-ctx.Done()
		return nil
	}

	// 自签模式
	if err := m.ensureCert(ctx); err != nil {
		return err
	}
	close(m.ready)

	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if m.expiresWithin(30 * 24 * time.Hour) {
				_ = m.ensureCert(ctx)
			}
		}
	}
}

// getCertificate 自签供给回调。
func (m *Manager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert == nil {
		return nil, errors.New("autotls: no certificate ready")
	}
	return m.cert, nil
}

// ensureCert 加载或签发自签证书并落盘。
func (m *Manager) ensureCert(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cert, err := m.load(); err == nil && !expiringSoon(cert, 30*24*time.Hour) {
		m.cert = cert
		return nil
	}
	cert, err := selfIssue(m.hosts)
	if err != nil {
		if old, lerr := m.load(); lerr == nil {
			m.cert = old
			return nil
		}
		return err
	}
	m.cert = cert
	return m.persist(cert)
}

// load 从磁盘加载自签证书对。
func (m *Manager) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(m.certFile(), m.keyFile())
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// persist 落盘自签证书对（cert 644 / key 600）。
func (m *Manager) persist(cert *tls.Certificate) error {
	if err := os.WriteFile(m.certFile(), pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: cert.Certificate[0],
	}), 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return err
	}
	return os.WriteFile(m.keyFile(), pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: keyDER,
	}), 0o600)
}

func hostPolicyOf(hosts []string) autocert.HostPolicy {
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[h] = true
	}
	return func(_ context.Context, host string) error {
		if allowed[host] {
			return nil
		}
		return errors.New("autotls: host not allowed: " + host)
	}
}

// ---------------------------------------------------------------- 标准库桥

// stdToFramework 标准 http.Handler → framework 处理器。
func stdToFramework(h http.Handler) framework.HandlerFunc {
	return func(c *framework.Context) {
		h.ServeHTTP(newStdW(c), newStdReq(c))
	}
}

// newStdW framework 上下文 → http.ResponseWriter。
type stdW struct{ c *framework.Context }

func newStdW(c *framework.Context) *stdW    { return &stdW{c: c} }
func (w *stdW) Header() http.Header         { return w.c.RespHeader() }
func (w *stdW) WriteHeader(code int)        { w.c.Status(code) }
func (w *stdW) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w *stdW) Flush()                      { w.c.Flush() }

// newStdReq framework 请求 → *http.Request。
func newStdReq(c *framework.Context) *http.Request {
	u := &url.URL{Path: c.Request.Path}
	if c.Request.RawQuery != "" {
		u.RawQuery = c.Request.RawQuery
	}
	r := &http.Request{
		Method:     c.Request.Method,
		URL:        u,
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

// ---------------------------------------------------------------- 自签实现

func selfIssue(hosts []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: firstOr(hosts, "framework-autotls")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func firstOr(list []string, def string) string {
	if len(list) > 0 {
		return list[0]
	}
	return def
}

func expiringSoon(cert *tls.Certificate, within time.Duration) bool {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return true
	}
	return time.Now().Add(within).After(leaf.NotAfter)
}

func (m *Manager) expiresWithin(d time.Duration) bool {
	m.mu.Lock()
	cert := m.cert
	m.mu.Unlock()
	if cert == nil {
		return true
	}
	return expiringSoon(cert, d)
}

func (m *Manager) certFile() string { return filepath.Join(m.dir, "cert.pem") }
func (m *Manager) keyFile() string  { return filepath.Join(m.dir, "key.pem") }
