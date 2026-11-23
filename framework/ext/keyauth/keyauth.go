// Package keyauth 基于 API-Key/Token 的认证中间件。
//
// 读取位置（按顺序探测）：Header(Key) → Bearer → query。
package keyauth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Config 认证配置。
type Config struct {
	// HeaderKey API-Key 头名，默认 "X-API-Key"。
	HeaderKey string

	// QueryKey 查询参数名，默认 "api_key"（最低优先级）。

	// AllowBearer 同时接受 "Authorization: Bearer <key>"。
	AllowBearer bool

	// Lookup 键校验器（返回身份标识；空串=未通过）。
	Lookup func(key string) (subject string, ok bool)

	// Unauth 自定义未认证行为（默认 401）。
	Unauth func(*framework.Context)
}

func (c *Config) withDefaults() {
	if c.HeaderKey == "" {
		c.HeaderKey = "X-API-Key"
	}
}

// Middleware 键认证。
func Middleware(cfg Config) framework.Middleware {
	cfg.withDefaults()
	unauth := cfg.Unauth
	if unauth == nil {
		unauth = defaultUnauth
	}
	return func(c *framework.Context) {
		if key := extract(c, &cfg); key != "" {
			if subject, ok := cfg.Lookup(key); ok && subject != "" {
				c.Set("framework.subject", subject)
				c.Next()
				return
			}
		}
		unauth(c)
	}
}

func extract(c *framework.Context, cfg *Config) string {
	if k := c.Header(cfg.HeaderKey); k != "" {
		return strings.TrimSpace(k)
	}
	if cfg.AllowBearer {
		auth := c.Header("Authorization")
		const prefix = "Bearer "
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
	}
	if q := c.Query("api_key"); q != "" {
		return q
	}
	return ""
}

func defaultUnauth(c *framework.Context) {
	c.Status(http.StatusUnauthorized)
	c.String(401, "401 Unauthorized")
	c.Abort()
}

// StaticKeys 静态键表校验器（常时比较）。
func StaticKeys(keys map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		for k, subject := range keys {
			if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
				return subject, true
			}
		}
		return "", false
	}
}

// Subject 取已认证身份（未认证为空串）。
func Subject(c *framework.Context) string {
	v, _ := c.Get("framework.subject")
	s, _ := v.(string)
	return s
}
