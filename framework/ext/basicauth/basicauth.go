// Package basicauth HTTP Basic 认证中间件。
//
// 策略：Realm 挑战 + 静态账号表或自定义校验器；凭据以常时比较。
package basicauth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Accounts 用户字典模式。
type Accounts map[string]string

// Config 认证配置。
type Config struct {
	Realm string // 挑战域，默认 "Restricted"
	// Unauth 未认证时的行为定制（默认 401+WWW-Authenticate）。
	Unauth func(*framework.Context)
}

func (c *Config) withDefaults() {
	if c.Realm == "" {
		c.Realm = "Restricted"
	}
}

// Middleware 静态账号表认证。
func Middleware(accounts Accounts, cfg Config) framework.Middleware {
	cfg.withDefaults()
	unauth := cfg.Unauth
	if unauth == nil {
		unauth = defaultUnauth(cfg.Realm)
	}
	return func(c *framework.Context) {
		user, pass, ok := c.Request.BasicAuth()
		if ok {
			if want, has := accounts[user]; has {
				if subtle.ConstantTimeCompare([]byte(want), []byte(pass)) == 1 {
					c.Set("framework.user", user)
					c.Next()
					return
				}
			}
		}
		unauth(c)
	}
}

// MiddlewareFunc 自定义校验器模式。
func MiddlewareFunc(realm string, validate func(user, pass string) bool) framework.Middleware {
	if realm == "" {
		realm = "Restricted"
	}
	unauth := defaultUnauth(realm)
	return func(c *framework.Context) {
		user, pass, ok := c.Request.BasicAuth()
		if ok && validate(user, pass) {
			c.Set("framework.user", user)
			c.Next()
			return
		}
		unauth(c)
	}
}

func defaultUnauth(realm string) func(*framework.Context) {
	return func(c *framework.Context) {
		c.SetRespHeader("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
		c.Status(http.StatusUnauthorized)
		c.String(401, "401 Unauthorized")
		c.Abort()
	}
}

// User 取已认证用户（未认证为空串）。
func User(c *framework.Context) string {
	v, _ := c.Get("framework.user")
	s, _ := v.(string)
	return s
}

var _ = strings.TrimSpace
