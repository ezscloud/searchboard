// Package static VFS 驱动的静态资源服务。
//
// 能力：MIME 探测（内置表）、强 ETag（内容 sha1 短截）+ If-None-Match 304、
// Range（单区间）、目录 index.html、SPA fallback（未命中且非 API 前缀时回退
// 指定文件如 /index.html）、Cache-Control。
package static

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/vfs"
)

// Options 静态服务选项。
type Options struct {
	// Index 目录命中时尝试的文件（默认 index.html）。
	Index string

	// SPA 未命中且路径不含 "." 时回退该文件（默认空=禁用）。
	SPA string

	// CacheControl 静态资源缓存头（默认 "no-cache"——ETag 协商为主）。
	CacheControl string

	// StripPrefix 路由前缀剥离（挂 /static/* 时传 /static）。
	StripPrefix string
}

func (o *Options) withDefaults() {
	if o.Index == "" {
		o.Index = "index.html"
	}
	if o.CacheControl == "" {
		o.CacheControl = "no-cache"
	}
	o.StripPrefix = strings.TrimSuffix(o.StripPrefix, "/")
}

// Handler 静态处理器（v 为资源栈）。
func Handler(v *vfs.Stack, opts Options) framework.HandlerFunc {
	opts.withDefaults()
	return func(c *framework.Context) {
		p := c.Request.Path
		if opts.StripPrefix != "" && strings.HasPrefix(p, opts.StripPrefix+"/") {
			p = strings.TrimPrefix(p, opts.StripPrefix)
		}
		if p == "" {
			p = "/"
		}
		if p == "/" {
			p = "/" + opts.Index
		}
		// 目录 → index
		if fi, err := v.Stat(p); err == nil && fi.IsDir {
			if !strings.HasSuffix(p, "/") {
				p += "/"
			}
			p += opts.Index
		}

		f, err := v.Open(p)
		if err != nil {
			// SPA 回退：路径无扩展名
			if opts.SPA != "" && !strings.Contains(path.Base(p), ".") {
				if rf, rerr := v.Open(opts.SPA); rerr == nil {
					serveFile(c, rf, opts.SPA, opts)
					return
				}
			}
			c.Error(http.StatusNotFound, "404 not found")
			return
		}
		serveFile(c, f, p, opts)
	}
}

// serveFile 输出单文件（ETag/304/Range/流式拷贝）。
func serveFile(c *framework.Context, f vfs.File, name string, opts Options) {
	defer f.Close()

	ext := strings.ToLower(path.Ext(name))
	ct := mimeOf(ext)
	c.SetRespHeader("Content-Type", ct)

	// 尺寸/ModTime 来自文件视图本身
	size := f.Size()
	mod := f.ModTime()

	// ETag：小文件内容哈希；大文件弱标签（尺寸+modtime）
	var etag string
	var body []byte
	if size >= 0 && size <= 4<<20 {
		body, _ = io.ReadAll(f)
		sum := sha1.Sum(body)
		etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	} else {
		etag = weakTagOf(size, mod)
	}
	if etag != "" {
		c.SetRespHeader("ETag", etag)
	}
	c.SetRespHeader("Cache-Control", opts.CacheControl)
	if v := c.RespHeader().Get("Vary"); v == "" {
		c.SetRespHeader("Vary", "Accept-Encoding")
	}

	// 304 协商
	if inm := c.Header("If-None-Match"); inm != "" && matchETag(inm, etag) {
		c.SetRespHeader("Content-Length", "0")
		c.Status(http.StatusNotModified)
		return
	}

	// Range（单区间；仅对全量已读的小文件可靠支持）
	if rng := c.Header("Range"); rng != "" && body != nil {
		if start, end, ok := parseRange(rng, int64(len(body))); ok {
			c.SetRespHeader("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+
				strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(int64(len(body)), 10))
			c.SetRespHeader("Accept-Ranges", "bytes")
			c.Status(http.StatusPartialContent)
			c.SetRespHeader("Content-Length", strconv.Itoa(int(end-start+1)))
			_, _ = c.Write(body[start : end+1])
			return
		}
	}

	c.SetRespHeader("Accept-Ranges", "bytes")
	if size >= 0 {
		c.SetRespHeader("Content-Length", strconv.FormatInt(size, 10))
	}
	c.Status(http.StatusOK)
	if body != nil {
		_, _ = c.Write(body)
		return
	}
	// 大文件流式拷贝
	_, _ = io.Copy(c, f)
}

func weakTagOf(size int64, mod time.Time) string {
	return `W/"s` + strconv.FormatInt(size, 10) + `-m` + strconv.FormatInt(mod.Unix(), 10) + `"`
}

func matchETag(inm, cur string) bool {
	if inm == "*" || inm == cur {
		return true
	}
	cur = strings.TrimPrefix(cur, "W/")
	for _, v := range strings.Split(inm, ",") {
		if strings.TrimPrefix(strings.TrimSpace(v), "W/") == cur {
			return true
		}
	}
	return false
}

// parseRange 解析 "bytes=a-b"（单区间）。
func parseRange(v string, size int64) (int64, int64, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(v, "bytes="))
	a, b, ok := strings.Cut(v, "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" { // 后缀 -N
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 || n > size {
			return 0, 0, false
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if b == "" {
		return start, size - 1, true
	}
	end, err := strconv.ParseInt(b, 10, 64)
	if err != nil || end < start || end >= size {
		if err == nil || b == "" {
			return start, size - 1, true
		}
		return 0, 0, false
	}
	return start, end, true
}

// mimeOf 扩展名 → MIME（内置表 + 标准库兜底）。
func mimeOf(ext string) string {
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml":
		return "application/xml"
	case ".map":
		return "application/json"
	case ".wasm":
		return "application/wasm"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
