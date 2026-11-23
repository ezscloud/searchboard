// Package csrf 提供 CSRF 防护（double-submit cookie 模式）。
//
// 机制：
//   - 每次安全方法请求（GET/HEAD/OPTIONS）下发随机令牌 cookie（HttpOnly=false，
//     供 JS 读取回填）；
//   - 不安全方法（POST/PUT/DELETE/PATCH）要求请求头 X-CSRF-Token 与 cookie 值一致；
//   - SameSite=Lax cookie + 令牌比对双保险（浏览器跨站表单无法携带自定义头）。
package csrf

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Config CSRF 配置。
type Config struct {
	CookieName string // 默认 "csrf_token"
	HeaderName string // 默认 "X-CSRF-Token"
	Secret     []byte // HMAC 密钥（可选；设置后 cookie 值带 HMAC 签名）
	CookiePath string // 默认 "/"
}

func (c *Config) withDefaults() {
	if c.CookieName == "" {
		c.CookieName = "csrf_token"
	}
	if c.HeaderName == "" {
		c.HeaderName = "X-CSRF-Token"
	}
	if c.CookiePath == "" {
		c.CookiePath = "/"
	}
}

// Middleware CSRF 防护。
func Middleware(cfg Config) framework.Middleware {
	cfg.withDefaults()
	return func(c *framework.Context) {
		if isSafeMethod(c.Request.Method) {
			issue(c, &cfg)
			c.Next()
			return
		}
		cookie := cookieOf(c, cfg.CookieName)
		header := c.Header(cfg.HeaderName)
		if cookie == "" || header == "" || !validPair(cfg.Secret, cookie, header) {
			c.Error(http.StatusForbidden, "CSRF token mismatch")
			c.Abort()
			return
		}
		issue(c, &cfg) // 滚动换发（可选策略；旧值未过期前浏览器仍持有）
		c.Next()
	}
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// issue 下发令牌 cookie（幂等：已持有且签名合法则不重发）。
func issue(c *framework.Context, cfg *Config) {
	v := cookieOf(c, cfg.CookieName)
	if v != "" && validPair(cfg.Secret, v, v) {
		return
	}
	tok := newToken(cfg.Secret)
	c.SetRespHeader("Set-Cookie", cookieString(cfg, tok))
	c.Set("framework.csrf.token", tok)
}

// CookieAll 取全部 Cookie 头值（framework Header.Get 只回首个）。
func CookieAll(c *framework.Context) []string {
	return c.Request.Header.Values("Cookie")
}

func cookieOf(c *framework.Context, name string) string {
	for _, line := range CookieAll(c) {
		for _, part := range strings.Split(line, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && k == name {
				return v
			}
		}
	}
	return ""
}

// newToken 生成令牌：明文或 "值.签名"。
func newToken(secret []byte) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	if len(secret) == 0 {
		return v
	}
	return v + "." + sign(secret, v)
}

// validPair 校验 cookie 值与 header 值（HMAC 模式同时恒时验签）。
func validPair(secret []byte, cookieV, headerV string) bool {
	if cookieV == "" || cookieV != headerV {
		return false
	}
	if len(secret) == 0 {
		return true
	}
	v, sig, ok := strings.Cut(cookieV, ".")
	if !ok || v == "" {
		return false
	}
	return verify(secret, v, sig)
}

func cookieString(cfg *Config, tok string) string {
	var b strings.Builder
	b.WriteString(cfg.CookieName + "=" + tok)
	b.WriteString("; Path=" + cfg.CookiePath)
	b.WriteString("; SameSite=Lax")
	// 令牌需被 JS 读取回填请求头，故不设 HttpOnly
	return b.String()
}
