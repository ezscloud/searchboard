package framework

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/ezscloud/searchboard/framework/h3"
)

// selfSignedForTest 测试用自签证书（复用 h3 包实现）。
func selfSignedForTest(host string) (tls.Certificate, error) {
	return h3.GenerateSelfSignedCert([]string{host})
}

// dialPlain 明文拨号（h2c 客户端用）。
func dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// dialPlainTimeout 带超时明文拨号。
func dialPlainTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

// netConn 测试别名。
type netConn = net.Conn
