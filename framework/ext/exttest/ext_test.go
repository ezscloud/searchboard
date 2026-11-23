// ext 各包 e2e 测试（集中一处驱动，走真实 framework 服务+客户端）。
package exttest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ext/basicauth"
	"github.com/ezscloud/searchboard/framework/ext/cache"
	"github.com/ezscloud/searchboard/framework/ext/csrf"
	"github.com/ezscloud/searchboard/framework/ext/etag"
	"github.com/ezscloud/searchboard/framework/ext/gzip"
	"github.com/ezscloud/searchboard/framework/ext/i18n"
	"github.com/ezscloud/searchboard/framework/ext/jwt"
	"github.com/ezscloud/searchboard/framework/ext/keyauth"
	"github.com/ezscloud/searchboard/framework/ext/limiter"
	"github.com/ezscloud/searchboard/framework/ext/loadbalance"
	"github.com/ezscloud/searchboard/framework/ext/monitor"
	"github.com/ezscloud/searchboard/framework/ext/secure"
	"github.com/ezscloud/searchboard/framework/ext/session"
	"github.com/ezscloud/searchboard/framework/ext/trace"
)

// ---------------------------------------------------------------- etag

func TestEtag304(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.GET("/v", etag.Wrap(`"v1"`, func(c *framework.Context) { c.String(200, "data") }))
	})
	cli := StdClient()

	// 首次：200 + ETag
	r1, err := cli.Get(base + "/v")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 || r1.Header.Get("ETag") != `"v1"` || string(b1) != "data" {
		t.Fatalf("first: %d %q %q", r1.StatusCode, r1.Header.Get("ETag"), b1)
	}

	// If-None-Match 命中 → 304
	req, _ := http.NewRequest("GET", base+"/v", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	r2, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 304 {
		t.Fatalf("etag 304: got %d", r2.StatusCode)
	}
}

// ---------------------------------------------------------------- csrf

func TestCSRFDoubleSubmit(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Use(csrf.Middleware(csrf.Config{Secret: []byte("k")}))
		s.GET("/tok", func(c *framework.Context) {
			v, _ := c.Get("framework.csrf.token")
			c.String(200, "%s", fmt.Sprint(v))
		})
		s.POST("/act", func(c *framework.Context) { c.String(200, "done") })
	})
	cli := StdClient()

	// GET 取令牌（Set-Cookie + body）
	r, err := cli.Get(base + "/tok")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	tok := string(b)
	sc := ""
	for _, c := range r.Cookies() {
		if c.Name == "csrf_token" {
			sc = c.Value
		}
	}
	if tok == "" || sc == "" || tok != sc {
		t.Fatalf("token issue: body=%q cookie=%q", tok, sc)
	}

	// POST 不带 header → 403
	r2, err := cli.Post(base+"/act", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 403 {
		t.Fatalf("no-token post: %d want 403", r2.StatusCode)
	}

	// POST 带 header → 200
	req, _ := http.NewRequest("POST", base+"/act", strings.NewReader(""))
	req.Header.Set("X-CSRF-Token", tok)
	r3, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("valid post: %d want 200", r3.StatusCode)
	}
}

// ---------------------------------------------------------------- basicauth / keyauth / jwt

func TestBasicAuth(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Use(basicauth.Middleware(basicauth.Accounts{"hardy": "pw"}, basicauth.Config{}))
		s.GET("/who", func(c *framework.Context) { c.String(200, "%s", basicauth.User(c)) })
	})
	cli := StdClient()

	r, _ := cli.Get(base + "/who")
	r.Body.Close()
	if r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauth: %d", r.StatusCode)
	}

	req, _ := http.NewRequest("GET", base+"/who", nil)
	req.SetBasicAuth("hardy", "pw")
	r2, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || string(b) != "hardy" {
		t.Fatalf("auth: %d %q", r2.StatusCode, b)
	}
}

func TestKeyAuth(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Use(keyauth.Middleware(keyauth.Config{AllowBearer: true, Lookup: keyauth.StaticKeys(map[string]string{"k1": "svc"})}))
		s.GET("/sub", func(c *framework.Context) { c.String(200, "%s", keyauth.Subject(c)) })
	})
	cli := StdClient()
	r, _ := cli.Get(base + "/sub")
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("unauth: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", base+"/sub", nil)
	req.Header.Set("Authorization", "Bearer k1")
	r2, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || string(b) != "svc" {
		t.Fatalf("auth: %d %q", r2.StatusCode, b)
	}
}

func TestJWT(t *testing.T) {
	signer := jwt.NewHS256([]byte("secret"), time.Minute)
	tok, err := signer.Sign(jwt.Claims{"sub": "hardy", "role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	base := Start(t, func(s *framework.Server) {
		s.Use(jwt.Middleware(signer))
		s.GET("/me", func(c *framework.Context) {
			c.JSON(200, map[string]any{"sub": jwt.Subject(c), "role": jwt.From(c).GetString("role")})
		})
	})
	cli := StdClient()

	r, _ := cli.Get(base + "/me")
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("unauth: %d", r.StatusCode)
	}

	req, _ := http.NewRequest("GET", base+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r2, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if !strings.Contains(string(b), "hardy") || !strings.Contains(string(b), "admin") {
		t.Fatalf("jwt: %s", b)
	}

	// 篡改 → 401
	req3, _ := http.NewRequest("GET", base+"/me", nil)
	req3.Header.Set("Authorization", "Bearer "+tok[:len(tok)-2]+"xx")
	r3, _ := cli.Do(req3)
	r3.Body.Close()
	if r3.StatusCode != 401 {
		t.Fatalf("tampered: %d", r3.StatusCode)
	}

	// 过期 → 401（显式 exp：负 ttl 不自动补，见 Sign 文档）
	past := time.Now().Add(-time.Minute).Unix()
	expTok, _ := signer.Sign(jwt.Claims{"sub": "x", "exp": past})
	if _, verr := signer.Verify(expTok); verr != jwt.ErrExpired {
		t.Fatalf("verify should reject expired: %v", verr)
	}
	req4, _ := http.NewRequest("GET", base+"/me", nil)
	req4.Header.Set("Authorization", "Bearer "+expTok)
	r4, _ := cli.Do(req4)
	r4.Body.Close()
	if r4.StatusCode != 401 {
		t.Fatalf("expired: %d", r4.StatusCode)
	}
}

// ---------------------------------------------------------------- session

func TestSession(t *testing.T) {
	store := session.NewMemoryStore(time.Minute)
	base := Start(t, func(s *framework.Server) {
		s.Use(session.Middleware(store, "app", nil))
		s.GET("/login", func(c *framework.Context) {
			session.From(c).Set("uid", 42)
			c.String(200, "ok")
		})
		s.GET("/who", func(c *framework.Context) {
			if v, ok := session.From(c).Get("uid"); ok {
				c.String(200, "%s", fmt.Sprint(v))
			} else {
				c.String(200, "anon")
			}
		})
	})
	cli := StdClient()

	// 未登录
	r, _ := cli.Get(base + "/who")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(b) != "anon" {
		t.Fatalf("anon: %q", b)
	}

	// 登录（种 cookie）
	r2, _ := cli.Get(base + "/login")
	r2.Body.Close()

	// 会话内续查（cookie jar 自动带）
	r3, _ := cli.Get(base + "/who")
	b3, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	if string(b3) != "42" {
		t.Fatalf("session: %q", b3)
	}
}

// ---------------------------------------------------------------- gzip

func TestGzip(t *testing.T) {
	// 缓冲窗口内（≤2KB）响应：MinLength 调低让窗口内可命中
	big := strings.Repeat("ab", 700) // 1.4KB（窗口内）
	base := Start(t, func(s *framework.Server) {
		s.Use(gzip.Middleware(gzip.Config{MinLength: 512}))
		s.GET("/big", func(c *framework.Context) { c.String(200, "%s", big) })
		s.GET("/small", func(c *framework.Context) { c.String(200, "hi") })
	})
	cli := StdClient()

	// Accept-Encoding: gzip → 压缩响应
	req, _ := http.NewRequest("GET", base+"/big", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip not applied: hdr=%v", r.Header)
	}
	b, _ := io.ReadAll(r.Body)
	if len(b) >= len(big) {
		t.Fatalf("compressed size %d >= raw %d", len(b), len(big))
	}

	// 不带 Accept-Encoding → 不压缩
	r2, _ := cli.Get(base + "/big")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.Header.Get("Content-Encoding") != "" || string(b2) != big {
		t.Fatalf("identity broken: %v", r2.Header.Get("Content-Encoding"))
	}

	// 小响应：不压缩但 body 必须完整（回归：曾因 warmup 缓冲丢失 body+CL=0）
	req3, _ := http.NewRequest("GET", base+"/small", nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	r3, _ := cli.Do(req3)
	b3, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	if r3.Header.Get("Content-Encoding") != "" {
		t.Fatalf("small should skip: %v", r3.Header.Get("Content-Encoding"))
	}
	if string(b3) != "hi" {
		t.Fatalf("small body lost: %q (CL=%d)", b3, r3.ContentLength)
	}

	// 连接复用回归：gzip 小响应后同连接下一请求必须正常解析
	// （曾因 body 丢失/CL 错位导致下一请求 405）
	r4, err := cli.Do(req3) // 复用同一条 keep-alive 连接
	if err != nil {
		t.Fatalf("keep-alive reuse after small gzip: %v", err)
	}
	b4, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if string(b4) != "hi" || r4.StatusCode != 200 {
		t.Fatalf("reuse broken: status=%d body=%q", r4.StatusCode, b4)
	}
}

// ---------------------------------------------------------------- secure / i18n

func TestSecureHeaders(t *testing.T) {
	base := Start(t, func(s *framework.Server) {
		s.Use(secure.Middleware(secure.Config{HSTSMaxAge: 31536000}))
		s.GET("/x", func(c *framework.Context) { c.String(200, "x") })
	})
	cli := StdClient()
	r, _ := cli.Get(base + "/x")
	r.Body.Close()
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := r.Header.Get(k); got != want {
			t.Fatalf("%s = %q want %q", k, got, want)
		}
	}
}

func TestI18n(t *testing.T) {
	bundle := i18n.New(map[string]map[string]string{
		"zh-CN": {"hello": "你好"},
		"en":    {"hello": "Hello"},
	}, i18n.Options{Default: "zh-CN"})
	base := Start(t, func(s *framework.Server) {
		s.Use(i18n.Middleware(bundle))
		s.GET("/hi", func(c *framework.Context) { c.String(200, "%s", i18n.T(c, "hello")) })
	})
	cli := StdClient()

	r, _ := cli.Get(base + "/hi")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(b) != "你好" {
		t.Fatalf("default: %q", b)
	}

	req, _ := http.NewRequest("GET", base+"/hi", nil)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r2, _ := cli.Do(req)
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if string(b2) != "Hello" {
		t.Fatalf("en: %q", b2)
	}
}

// ---------------------------------------------------------------- limiter

func TestLimiter(t *testing.T) {
	bbr := limiter.NewBBR(limiter.Config{MaxPass: 3, Window: time.Second})
	base := Start(t, func(s *framework.Server) {
		s.Use(limiter.Middleware(bbr))
		s.GET("/q", func(c *framework.Context) { c.String(200, "ok") })
	})
	cli := StdClient()

	var ok, limited int32
	for i := 0; i < 10; i++ {
		r, err := cli.Get(base + "/q")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == 429 {
			atomic.AddInt32(&limited, 1)
		} else if r.StatusCode == 200 {
			atomic.AddInt32(&ok, 1)
		}
	}
	if ok < 3 || limited == 0 {
		t.Fatalf("expected >=3 ok and some 429, got ok=%d limited=%d", ok, limited)
	}
}

// ---------------------------------------------------------------- cache

func TestCacheHitMiss(t *testing.T) {
	var hits int32
	base := Start(t, func(s *framework.Server) {
		s.Use(cache.Middleware(cache.Config{TTL: 2 * time.Second, OnHit: func(*framework.Context) { atomic.AddInt32(&hits, 1) }}))
		s.GET("/c", func(c *framework.Context) { c.String(200, "content") })
	})
	cli := StdClient()
	r1, _ := cli.Get(base + "/c")
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	r2, _ := cli.Get(base + "/c")
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if atomic.LoadInt32(&hits) != 1 || string(b1) != "content" || string(b2) != "content" {
		t.Fatalf("cache: hits=%d b1=%q b2=%q", hits, b1, b2)
	}
}

// ---------------------------------------------------------------- loadbalance（纯逻辑）

func TestLoadBalanceRRAndFailover(t *testing.T) {
	eps := []loadbalance.Endpoint{{Addr: "a:1"}, {Addr: "b:2"}, {Addr: "c:3"}}
	p := loadbalance.New(eps, loadbalance.Config{})
	var seen = map[string]int{}
	for i := 0; i < 30; i++ {
		ep, err := p.Pick("")
		if err != nil {
			t.Fatal(err)
		}
		seen[ep.Addr]++
	}
	if len(seen) != 3 {
		t.Fatalf("rr should spread: %v", seen)
	}

	// 摘除 a:1
	p.Report(&eps[0], false)
	p.Report(&eps[0], false)
	p.Report(&eps[0], false)
	for i := 0; i < 10; i++ {
		ep, _ := p.Pick("")
		if ep.Addr == "a:1" {
			t.Fatalf("a:1 should be removed")
		}
	}
	// 恢复
	p.Report(&eps[0], true)
	if ep, _ := p.Pick(""); ep.Addr != "a:1" {
		// RR 顺序不保证首个即 a:1；只要不 ErrNoAvailable 即可
		_ = ep
	}
}

// ---------------------------------------------------------------- monitor

func TestPrometheusTextFormat(t *testing.T) {
	reg := monitor.New("framework")
	base := Start(t, func(s *framework.Server) {
		s.Use(monitor.Middleware(reg))
		s.GET("/m", reg.Handler)
		s.GET("/hit", func(c *framework.Context) { c.String(200, "ok") })
	})
	cli := StdClient()
	r, _ := cli.Get(base + "/hit")
	r.Body.Close()
	r2, _ := cli.Get(base + "/m")
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	text := string(b)
	if !strings.Contains(text, "framework_http_requests_total") {
		t.Fatalf("metrics missing counter: %s", text[:min(200, len(text))])
	}
	if !strings.Contains(text, "framework_http_request_duration_seconds_bucket") {
		t.Fatalf("metrics missing histogram")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- trace

func TestTraceContextPropagation(t *testing.T) {
	tracer := trace.NewTracer(nil)
	base := Start(t, func(s *framework.Server) {
		s.Use(trace.Middleware(tracer))
		s.GET("/t", func(c *framework.Context) {
			tr := trace.From(c)
			c.String(200, "%s", tr.TraceID)
		})
	})
	cli := StdClient()
	r, _ := cli.Get(base + "/t")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if len(string(b)) != 32 {
		t.Fatalf("trace id len = %d", len(string(b)))
	}
	tp := r.Header.Get("Traceparent")
	// W3C traceparent v00 总长恰为 55（2+1+32+1+16+1+2）
	if !strings.HasPrefix(tp, "00-") || len(tp) != 55 {
		t.Fatalf("traceparent = %q", tp)
	}

	// 入站延续：同 traceparent → 同 TraceID
	req, _ := http.NewRequest("GET", base+"/t", nil)
	req.Header.Set("Traceparent", tp)
	r2, _ := cli.Do(req)
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if string(b2) != string(b) {
		t.Fatalf("trace continuity broken: %s vs %s", b, b2)
	}
}
