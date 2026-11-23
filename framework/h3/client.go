// h3/client.go：HTTP/3 客户端封装。
package h3

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"

	h3lib "github.com/quic-go/quic-go/http3"
)

// Client HTTP/3 客户端（底层 http3.Transport，按 host 复用 QUIC 连接）。
type Client struct {
	transport *h3lib.Transport
	tlsConf   *tls.Config
}

// NewClient 创建客户端；tlsConf 为 nil 时用宽松默认（仅适合测试：
// 自动 InsecureSkipVerify）。
func NewClient(tlsConf *tls.Config) *Client {
	if tlsConf == nil {
		tlsConf = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 测试便捷
	}
	return &Client{tlsConf: tlsConf}
}

// transport 惰性创建。
func (c *Client) rt() *h3lib.Transport {
	if c.transport == nil {
		c.transport = &h3lib.Transport{TLSClientConfig: c.tlsConf}
	}
	return c.transport
}

// RoundTrip 执行请求（复用底层 Transport）。
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	return c.rt().RoundTrip(req)
}

// Get 简单 GET（返回完整响应体）。
func (c *Client) Get(ctx context.Context, rawurl string) (status int, body []byte, hdr http.Header, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.rt().RoundTrip(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	return resp.StatusCode, b, resp.Header, nil
}

// Close 释放底层连接。
func (c *Client) Close() error {
	if c.transport != nil {
		return c.transport.Close()
	}
	return nil
}
