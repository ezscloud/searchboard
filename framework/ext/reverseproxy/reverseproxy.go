// Package reverseproxy 反向代理（对标 hertz-contrib/reverseproxy 简化版）。
//
// 能力：单/多上游（配合 ext/loadbalance）、Host 重写、Header 追加、
// 超时与重试（幂等方法）、上游摘除（Report 失败计数）。
package reverseproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ext/loadbalance"
)

// Config 代理配置。
type Config struct {
	// Targets 上游列表（静态模式）。
	Targets []loadbalance.Endpoint

	// Picker 动态均衡器（设置则忽略 Targets）。
	Picker loadbalance.Picker

	// RewriteHost 重写转发 Host（空=保留原始 Host）。
	RewriteHost string

	// AppendHeaders 追加到上游请求的头。
	AppendHeaders map[string]string

	// HopHeaders 去除的逐跳头（默认集 + 自定义追加）。
	HopHeaders []string

	// Timeout 上游总时限（默认 15s）。
	Timeout time.Duration

	// Retries 幂等方法（GET/HEAD）失败重试次数（默认 1）。
	Retries int

	// ErrorHandler 上游全失败时的响应定制（默认 502）。
	ErrorHandler func(c *framework.Context, err error)

	// Logf 调试日志。
	Logf func(format string, args ...any)
}

func (c *Config) withDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.Retries < 0 {
		c.Retries = 1
	}
	if c.ErrorHandler == nil {
		c.ErrorHandler = func(ctx *framework.Context, err error) {
			ctx.SetRespHeader("X-Proxy-Error", sanitizeErr(err))
			ctx.Status(http.StatusBadGateway)
			_, _ = ctx.WriteString("502 Bad Gateway")
		}
	}
}

var defaultHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Handler 代理处理器。
func Handler(cfg Config) framework.HandlerFunc {
	cfg.withDefaults()
	hop := map[string]bool{}
	for _, h := range defaultHopHeaders {
		hop[strings.ToLower(h)] = true
	}
	for _, h := range cfg.HopHeaders {
		hop[strings.ToLower(h)] = true
	}

	return func(c *framework.Context) {
		var picker loadbalance.Picker
		if cfg.Picker != nil {
			picker = cfg.Picker
		} else {
			picker = loadbalance.New(cfg.Targets, loadbalance.Config{Strategy: loadbalance.RoundRobin})
		}

		key := c.ClientIP()
		if c.Request.Proto == "HTTP/3" || c.Request.Proto == "HTTP/2.0" {
			key = c.Header("X-Request-ID")
		}

		attempts := 1
		if isIdempotent(c.Request.Method) {
			attempts += cfg.Retries
		}

		var lastErr error
		for i := 0; i < attempts; i++ {
			ep, err := picker.Pick(key)
			if err != nil {
				lastErr = err
				break
			}
			resp, err := forward(c, ep.Addr, &cfg, hop)
			if err != nil {
				lastErr = err
				picker.Report(ep, false)
				continue
			}
			picker.Report(ep, true)
			relay(c, resp)
			return
		}
		cfg.ErrorHandler(c, lastErr)
	}
}

// forward 建立上游连接并写出请求（framework.Client 同源编解码）。
func forward(c *framework.Context, addr string, cfg *Config, hop map[string]bool) (*httpResponse, error) {
	ctx, cancel := context.WithTimeout(c2ctx(c), cfg.Timeout)
	defer cancel()

	// 上游 URL：scheme 固定 http（上游 TLS 场景给 RewriteHost+Targets 走 wss 类配置扩展）
	scheme := "http"
	if cfg2 := c.Header("X-Upstream-Scheme"); strings.HasPrefix(cfg2, "http") {
		scheme = cfg2 // 调试/网关链路允许指定
	}
	target := scheme + "://" + addr + c.Request.Target

	body, err := c.ReadBody(64 << 20)
	if err != nil {
		return nil, err
	}
	var bodyR io.Reader
	if len(body) > 0 {
		bodyR = bytes.NewReader(body)
	}

	req := newClientRequest(ctx, c.Request.Method, target, bodyR)
	if bodyR != nil {
		req.BodyLen = int64(len(body))
	}
	// 头复制（去逐跳 + 追加）
	for k, vs := range c.Request.Header {
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	host := cfg.RewriteHost
	if host == "" {
		host = addr
	}
	req.Header.Set("Host", host)
	for k, v := range cfg.AppendHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Forwarded-For", forwardedFor(c))
	if req.Header.Get("X-Forwarded-Proto") == "" {
		if c.IsTLS() {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}

	resp, err := clientDo(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// relay 把上游响应中继给客户端（头 + 状态 + 体）。
func relay(c *framework.Context, resp *httpResponse) {
	for k, vs := range resp.Header {
		lower := strings.ToLower(k)
		if lower == "connection" || lower == "transfer-encoding" || lower == "content-length" {
			continue
		}
		for _, v := range vs {
			c.RespHeader().Add(k, v)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c, resp.Body())
	resp.Close()
}

func isIdempotent(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func forwardedFor(c *framework.Context) string {
	prior := c.Header("X-Forwarded-For")
	if prior != "" {
		return prior + ", " + c.ClientIP()
	}
	return c.ClientIP()
}

func sanitizeErr(err error) string {
	if err == nil {
		return "unknown"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// c2ctx 请求上下文桥。
func c2ctx(c *framework.Context) context.Context { return context.Background() }

var _ = fmt.Sprintf
