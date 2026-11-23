package exttest

import (
	"embed"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ext/static"
	"github.com/ezscloud/searchboard/framework/vfs"
)

//go:embed testdata/public
var publicFS embed.FS

// testStack 构造 embed 基座 + 磁盘覆盖层栈。
func testStack(t *testing.T) *vfs.Stack {
	t.Helper()
	over := t.TempDir()
	// 覆盖 index.html（自定义品牌场景）
	if err := os.WriteFile(filepath.Join(over, "index.html"), []byte("custom-brand-page"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 覆盖 logo（子目录需先建）
	if err := os.MkdirAll(filepath.Join(over, "brand"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(over, "brand", "logo.svg"), []byte("<svg>over</svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return vfs.NewStack(vfs.DirLayer(over), vfs.EmbedLayer(publicFS, "testdata/public"))
}

func TestStaticOverlayAndETag(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Any("/*p", static.Handler(testStack(t), static.Options{SPA: "/index.html"}))
	})
	cli := StdClient()

	// 覆盖层 index.html 生效（非基座内容）
	r, _ := cli.Get(base + "/")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || string(b) != "custom-brand-page" {
		t.Fatalf("index overlay: %d %q", r.StatusCode, b)
	}

	// 基座文件
	r2, _ := cli.Get(base + "/app.js")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || !strings.Contains(string(b2), "console.log") {
		t.Fatalf("base file: %d %q", r2.StatusCode, b2)
	}
	if r2.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("mime: %v", r2.Header.Get("Content-Type"))
	}

	// ETag + 304
	et := r2.Header.Get("ETag")
	if et == "" {
		t.Fatalf("missing etag")
	}
	req, _ := http.NewRequest("GET", base+"/app.js", nil)
	req.Header.Set("If-None-Match", et)
	r3, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 304 {
		t.Fatalf("etag 304: %d", r3.StatusCode)
	}

	// 覆盖层 logo
	r4, _ := cli.Get(base + "/brand/logo.svg")
	b4, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if r4.StatusCode != 200 || string(b4) != "<svg>over</svg>" {
		t.Fatalf("logo overlay: %d %q", r4.StatusCode, b4)
	}
}

func TestStaticRangeAndSPA(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Any("/*p", static.Handler(testStack(t), static.Options{SPA: "/index.html"}))
	})
	cli := StdClient()

	// Range 请求（基座 app.js 全量已知）
	req, _ := http.NewRequest("GET", base+"/app.js", nil)
	req.Header.Set("Range", "bytes=0-9")
	r, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusPartialContent || len(b) != 10 {
		t.Fatalf("range: %d %d bytes", r.StatusCode, len(b))
	}
	if r.Header.Get("Content-Range") == "" {
		t.Fatalf("missing content-range")
	}

	// SPA 回退：无扩展名未命中 → index.html
	r2, _ := cli.Get(base + "/some/route")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || string(b2) != "custom-brand-page" {
		t.Fatalf("spa fallback: %d %q", r2.StatusCode, b2)
	}

	// 真不存在（带扩展名）→ 404
	r3, _ := cli.Get(base + "/nope.png")
	r3.Body.Close()
	if r3.StatusCode != 404 {
		t.Fatalf("missing: %d", r3.StatusCode)
	}
}
