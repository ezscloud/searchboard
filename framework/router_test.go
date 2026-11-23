package framework

import (
	"net/http"
	"testing"
)

func newTestRouter() *Router {
	r := NewRouter()
	r.GET("/", func(c *Context) { c.String(200, "root") })
	r.GET("/hello/:name", func(c *Context) { c.String(200, "hello %s", c.Param("name")) })
	r.GET("/users/:uid/posts/:pid", func(c *Context) {
		c.String(200, "%s/%s", c.Param("uid"), c.Param("pid"))
	})
	r.GET("/static/*filepath", func(c *Context) { c.String(200, "file:%s", c.Param("filepath")) })
	r.POST("/hello", func(c *Context) { c.String(201, "posted") })
	return r
}

func TestRouterStaticAndParams(t *testing.T) {
	r := newTestRouter()
	cases := []struct {
		path   string
		method string
		want   string
		found  bool
	}{
		{"/", "GET", "root", true},
		{"/hello/webfw", "GET", "hello webfw", true},
		{"/hello/a/b", "GET", "", false},
		{"/users/7/posts/9", "GET", "7/9", true},
		{"/static/css/main.css", "GET", "file:css/main.css", true},
		{"/static/", "GET", "file:", true}, // 尾斜杠：*filepath 捕获空串
		{"/static", "GET", "", false},      // 无尾斜杠不匹配（严格模式，对齐 httprouter）
		{"/static/a/b/c.js", "GET", "file:a/b/c.js", true},
		{"/nope", "GET", "", false},
		{"/hello", "POST", "posted", true},
	}
	for _, tc := range cases {
		h, params, _ := r.Lookup(tc.method, tc.path)
		if tc.found && h == nil {
			t.Fatalf("%s %s: want match, got none", tc.method, tc.path)
		}
		if !tc.found {
			continue
		}
		c := &Context{Params: params, Request: &Request{Method: tc.method, Path: tc.path},
			sink: &captureSink{}}
		h(c)
		if s := captureBody(c); s != tc.want {
			t.Errorf("%s %s: body = %q, want %q", tc.method, tc.path, s, tc.want)
		}
	}
}

func TestRouter405Allow(t *testing.T) {
	r := newTestRouter()
	h, _, allow := r.Lookup("DELETE", "/hello/x")
	if h != nil {
		t.Fatalf("DELETE should not match")
	}
	if len(allow) == 0 {
		t.Fatalf("expected allow list for 405")
	}
	found := false
	for _, m := range allow {
		if m == "GET" {
			found = true
		}
	}
	if !found {
		t.Errorf("allow list %v should contain GET", allow)
	}
}

func TestRouterHEADFallbackGET(t *testing.T) {
	r := newTestRouter()
	h, params, _ := r.Lookup(http.MethodHead, "/hello/abc")
	if h == nil {
		t.Fatalf("HEAD should fall back to GET handler")
	}
	if params["name"] != "abc" {
		t.Errorf("params = %v", params)
	}
}

func TestRouterConflict(t *testing.T) {
	r := newTestRouter()
	if err := r.Handle("GET", "/hello/:name", func(c *Context) {}); err == nil {
		t.Fatalf("duplicate registration should fail")
	}
	if err := r.Handle("GET", "/dup", nil); err == nil {
		t.Fatalf("nil handler should fail")
	}
	if err := r.Handle("GET", "/bad/*rest/x", func(c *Context) {}); err == nil {
		t.Fatalf("wildcard not last should fail")
	}
	if err := r.Handle("GET", "no-slash", func(c *Context) {}); err == nil {
		t.Fatalf("path without leading slash should fail")
	}
}

func TestRouterParamDecode(t *testing.T) {
	// 参数解码发生在请求解析层（readRequest 对整路径做 PathUnescape），
	// 路由层只看已解码路径；这里直接验证解码后的路径匹配。
	r := NewRouter()
	r.GET("/p/:v", func(c *Context) { c.String(200, "%s", c.Param("v")) })
	h, params, _ := r.Lookup("GET", "/p/中文")
	if h == nil {
		t.Fatalf("no match")
	}
	if params["v"] != "中文" {
		t.Errorf("param = %q, want 中文", params["v"])
	}
}

// captureSink / captureBody 测试辅助：捕获写出。
type captureSink struct {
	body   []byte
	status int
	hdr    http.Header
}

func (k *captureSink) Header() http.Header {
	if k.hdr == nil {
		k.hdr = make(http.Header)
	}
	return k.hdr
}
func (k *captureSink) WriteHeader(code int)        { k.status = code }
func (k *captureSink) Write(p []byte) (int, error) { k.body = append(k.body, p...); return len(p), nil }
func (k *captureSink) Flush()                      {}
func (k *captureSink) Status() int {
	if k.status == 0 {
		return 200
	}
	return k.status
}
func (k *captureSink) Size() int64      { return int64(len(k.body)) }
func (k *captureSink) Hijacked() bool   { return false }
func (k *captureSink) MarkHijacked()    {}
func (k *captureSink) Finish() error    { return nil }
func (k *captureSink) Snapshot() []byte { return k.body }
func (k *captureSink) Rewrite(b []byte) bool {
	k.body = append(k.body[:0], b...)
	return true
}

func captureBody(c *Context) string { return string(c.sink.(*captureSink).body) }
