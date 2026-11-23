// middleware.go：内置中间件（Next 语义：续链调用 c.Next()）。
//
//	Recovery  panic → 500（必装建议：路由注册顺序最前）
//	Logger    请求日志（method/path/status/size/duration/remote）
//	RequestID X-Request-ID 透传或生成
//	CORS      跨域（预检 + 简单请求）
package framework

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------- Recovery

// Recovery panic 恢复：记录堆栈并回 500。
// out 为 nil 时不落日志（生产建议给 io.Writer）。
func Recovery(out io.Writer) Middleware {
	return func(c *Context) {
		defer func() {
			if r := recover(); r != nil {
				if out != nil {
					fmt.Fprintf(out, "[framework] panic: %v\n%s\n", r, stackTrace())
				}
				if !c.sinkWrote() {
					c.Status(http.StatusInternalServerError)
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

// sinkWrote 响应是否已开始（决定 panic 时能否补 500）。
func (c *Context) sinkWrote() bool { return c.CurrentStatus() != 0 }

func stackTrace() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// ---------------------------------------------------------------- Logger

// Logger 请求日志中间件。
// out 为 nil 时静默（只统计不输出）。
func Logger(out io.Writer) Middleware {
	return func(c *Context) {
		start := time.Now()
		c.Next()
		if out == nil {
			return
		}
		fmt.Fprintf(out, "[framework] %s %s -> %d %dB %s (%s)\n",
			c.Request.Method, c.Request.Path,
			statusOr(c.CurrentStatus()), c.ResponseSize(),
			time.Since(start).Round(time.Microsecond), c.ClientIP())
	}
}

func statusOr(s int) int {
	if s == 0 {
		return 200
	}
	return s
}

// ---------------------------------------------------------------- RequestID

// RequestID 透传或生成 X-Request-ID（16 字节随机 hex），并存入 ctx。
func RequestID() Middleware {
	return func(c *Context) {
		id := c.Header("X-Request-ID")
		if id == "" {
			id = randHex16()
		}
		c.Set("requestID", id)
		c.SetRespHeader("X-Request-ID", id)
		c.Next()
	}
}

func randHex16() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- CORS

// CORSConfig 跨域配置。
type CORSConfig struct {
	AllowOrigins     []string // 空或含 "*" → 回显请求 Origin
	AllowMethods     []string // 默认 GET/POST/PUT/DELETE/PATCH/OPTIONS/HEAD
	AllowHeaders     []string // 默认回显请求的 Access-Control-Request-Headers
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           int // 预检缓存秒数，默认 600
}

// CORS 跨域中间件。
func CORS(cfg CORSConfig) Middleware {
	if len(cfg.AllowMethods) == 0 {
		cfg.AllowMethods = []string{
			http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodDelete, http.MethodPatch, http.MethodOptions, http.MethodHead,
		}
	}
	methods := strings.Join(cfg.AllowMethods, ", ")
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 600
	}
	return func(c *Context) {
		origin := c.Header("Origin")
		if origin == "" {
			c.Next()
			return
		}
		if !originAllowed(cfg.AllowOrigins, origin) {
			c.Next()
			return
		}
		h := c.RespHeader()
		if isWildcard(cfg.AllowOrigins) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
		} else {
			h.Set("Access-Control-Allow-Origin", strings.Join(cfg.AllowOrigins, ", "))
		}
		if cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if len(cfg.ExposeHeaders) > 0 {
			h.Set("Access-Control-Expose-Headers", strings.Join(cfg.ExposeHeaders, ", "))
		}

		if c.Request.Method == http.MethodOptions &&
			c.Header("Access-Control-Request-Method") != "" {
			// 预检
			h.Set("Access-Control-Allow-Methods", methods)
			if reqHdrs := c.Header("Access-Control-Request-Headers"); reqHdrs != "" {
				h.Set("Access-Control-Allow-Headers", reqHdrs)
			} else if len(cfg.AllowHeaders) > 0 {
				h.Set("Access-Control-Allow-Headers", strings.Join(cfg.AllowHeaders, ", "))
			}
			h.Set("Access-Control-Max-Age", fmt.Sprintf("%d", maxAge))
			c.Status(http.StatusNoContent)
			c.Abort()
			return
		}
		c.Next()
	}
}

func isWildcard(origins []string) bool {
	if len(origins) == 0 {
		return true
	}
	for _, o := range origins {
		if o == "*" {
			return true
		}
	}
	return false
}

func originAllowed(allowed []string, origin string) bool {
	if isWildcard(allowed) {
		return true
	}
	for _, a := range allowed {
		if a == origin {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 超时

// Timeout 处理器执行时限：超时仅终止链上后续处理器（协作式，不抢占
// 长阻塞 IO；流式响应慎用）。
func Timeout(d time.Duration) Middleware {
	return func(c *Context) {
		timer := time.AfterFunc(d, func() {
			if !c.sinkWrote() {
				c.Status(http.StatusServiceUnavailable)
				c.Abort()
			}
		})
		defer timer.Stop()
		c.Next()
	}
}
