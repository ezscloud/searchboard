// Package secure 提供安全响应头中间件（对标 hertz-contrib/secure）。
//
// 默认头集：
//   - X-Content-Type-Options: nosniff
//   - X-Frame-Options: SAMEORIGIN
//   - X-XSS-Protection: 1; mode=block（旧浏览器）
//   - Referrer-Policy: strict-origin-when-cross-origin
//   - Content-Security-Policy（可配）
//   - Strict-Transport-Security（TLS 链路，可配 maxAge）
package secure

import (
	"strconv"

	"github.com/ezscloud/searchboard/framework"
)

// Config 安全头配置。
type Config struct {
	// ContentSecurityPolicy 自定义 CSP；空=不下发。
	ContentSecurityPolicy string

	// HSTSMaxAge HSTS 秒数（仅 TLS 链路下发；0=不下发）。
	HSTSMaxAge int

	// FrameOptions 默认 SAMEORIGIN；可设 DENY / 空串（禁用）。
	FrameOptions string

	// AllowedHosts Host 白名单；非空时请求 Host 不在名单内 → 400。
	AllowedHosts []string

	// SSLRedirect http→https 跳转开关（配 SSLHost 生效）。
	SSLRedirect bool
	SSLHost     string
}

// Middleware 安全头中间件。
func Middleware(cfg Config) framework.Middleware {
	if cfg.FrameOptions == "" {
		cfg.FrameOptions = "SAMEORIGIN"
	}
	hostSet := map[string]bool{}
	for _, h := range cfg.AllowedHosts {
		hostSet[h] = true
	}
	return func(c *framework.Context) {
		if len(hostSet) > 0 && !hostSet[c.Request.Host] {
			c.Error(400, "host not allowed")
			c.Abort()
			return
		}
		if cfg.SSLRedirect && !c.IsTLS() {
			host := cfg.SSLHost
			if host == "" {
				host = c.Request.Host
			}
			if host != "" {
				c.Redirect(301, "https://"+host+c.Request.Target)
				c.Abort()
				return
			}
		}
		h := c.RespHeader()
		h.Set("X-Content-Type-Options", "nosniff")
		if cfg.FrameOptions != "" {
			h.Set("X-Frame-Options", cfg.FrameOptions)
		}
		h.Set("X-XSS-Protection", "1; mode=block")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if cfg.ContentSecurityPolicy != "" {
			h.Set("Content-Security-Policy", cfg.ContentSecurityPolicy)
		}
		if cfg.HSTSMaxAge > 0 && c.IsTLS() {
			h.Set("Strict-Transport-Security", "max-age="+strconv.Itoa(cfg.HSTSMaxAge))
		}
		c.Next()
	}
}
