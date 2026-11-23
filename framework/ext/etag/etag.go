// Package etag 提供 ETag 协商缓存。
//
// 两种用法：
//   - Wrap(tag, handler)：声明式（推荐）。请求 If-None-Match 命中 tag →
//     直接 304 短路（处理器不执行、零响应体）；未命中 → 执行处理器并
//     回写 ETag 头。
//   - Middleware()：对已声明 c.Set("framework.etag", tag) 的请求做同样短路。
//
// 自动按响应体生成 ETag 需要捕获已提交响应，v1 不做（设计取舍见 README）。
package etag

import (
	"net/http"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// key 上下文存取键（Middleware 用）。
const Key = "framework.etag"

// Wrap 声明式 ETag 处理器包装。
//
// tag 形如 `"a1b2c3"`（强）或 W/"v1"（弱）；业务通常以资源版本号生成。
func Wrap(tag string, h framework.HandlerFunc) framework.HandlerFunc {
	return func(c *framework.Context) {
		if Match(c.Header("If-None-Match"), tag) {
			c.SetRespHeader("ETag", tag)
			c.Status(http.StatusNotModified)
			return
		}
		h(c)
		if c.RespHeader().Get("ETag") == "" {
			c.SetRespHeader("ETag", tag)
		}
	}
}

// Middleware 声明式 ETag：处理器内 c.Set(etag.Key, tag) 即参与协商。
func Middleware() framework.Middleware {
	return func(c *framework.Context) {
		c.Next()
		v, ok := c.Get(Key)
		if !ok {
			return
		}
		tag, _ := v.(string)
		if tag == "" || c.CurrentStatus() >= 300 {
			return
		}
		if c.RespHeader().Get("ETag") == "" {
			c.SetRespHeader("ETag", tag)
		}
	}
}

// MiddlewarePre 前置短路版：声明 tag 由上游中间件（如缓存层）注入
// c.Set(Key, tag) 后，本中间件在处理器前执行协商。
func MiddlewarePre() framework.Middleware {
	return func(c *framework.Context) {
		v, _ := c.Get(Key)
		tag, _ := v.(string)
		if tag != "" && Match(c.Header("If-None-Match"), tag) {
			c.SetRespHeader("ETag", tag)
			c.Status(http.StatusNotModified)
			c.Abort()
			return
		}
		c.Next()
	}
}

// Match If-None-Match 匹配（支持 * / 列表 / 单值，弱比较忽略 W/ 前缀）。
func Match(inm, current string) bool {
	if inm == "" || current == "" {
		return false
	}
	if inm == "*" {
		return true
	}
	cur := strings.TrimPrefix(current, "W/")
	for _, v := range strings.Split(inm, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		v = strings.TrimPrefix(v, "W/")
		if v == cur {
			return true
		}
	}
	return false
}
