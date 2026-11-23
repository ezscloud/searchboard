// Package api searchboard HTTP API。
//
// 路由清单（全部挂在 /api/*，前端与静态资源走 /public/* 与 SPA fallback）：
//
//	GET  /api/state        启动状态（版本/品牌/日志目录/ES 概要）
//	GET  /api/health       ES 探活
//	GET  /api/es/root      透传 GET /
//	GET  /api/es/health    透传 _cluster/health
//	GET  /api/es/stats     透传 _cluster/stats
//	GET  /api/es/nodes     透传 _nodes/stats
//	GET  /api/es/indices   透传 _cat/indices?format=json&bytes=b
//	GET  /api/es/shards    透传 _cat/shards?format=json&bytes=b
//	POST /api/es/rest      透传任意 REST（body: {method,path,body}）
//	GET  /api/logs         日志文件列表
//	GET  /api/logs/stream  SSE 实时日志流（?file=&lines=）
//	GET  /api/brand        当前品牌
//	POST /api/brand        运行时更新品牌（内存态；重启回落到 YAML）
//
// 安全说明：本服务定位为本地/内网管理台，默认无鉴权；如需暴露公网，
// 建议前置反向代理加 BasicAuth/IP 白名单（es 密码只存在于本进程内存与
// 配置文件，不落浏览器）。
package api

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/jsonx"
	"github.com/ezscloud/searchboard/searchboard/internal/es"
	"github.com/ezscloud/searchboard/searchboard/internal/logtail"
)

// Brand 品牌定制信息。
type Brand struct {
	Name   string `json:"name"`
	Slogan string `json:"slogan"`
	Logo   string `json:"logo"` // 空=前端用内置默认
}

// BrandState 运行时品牌状态（读写锁保护；POST 更新立即生效，Reset 回落 YAML）。
type BrandState struct {
	mu  sync.RWMutex
	def Brand // YAML 配置默认
	cur Brand // 运行时当前
}

// NewBrandState 初始化（来自 YAML 配置）。
func NewBrandState(b Brand) *BrandState { return &BrandState{def: b, cur: b} }

// Get 当前品牌。
func (s *BrandState) Get() Brand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Set 更新品牌（空 Name/Slogan 保留原值；Logo 直接覆盖，空串=清除改由前端自动探测）。
func (s *BrandState) Set(b Brand) Brand {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.Name != "" {
		s.cur.Name = b.Name
	}
	if b.Slogan != "" {
		s.cur.Slogan = b.Slogan
	}
	s.cur.Logo = b.Logo
	return s.cur
}

// Reset 恢复 YAML 默认。
func (s *BrandState) Reset() Brand {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = s.def
	return s.cur
}

// Options API 装配选项。
type Options struct {
	ES      *es.Client
	Tail    *logtail.Tailer
	Brand   *BrandState
	Version string

	// HeartbeatInterval SSE 心跳间隔（默认 15s）。心跳注释行写失败即感知
	// 客户端断开——这是长连接断开感知的可靠机制（不探测连接读方向）。
	HeartbeatInterval time.Duration
}

// API 路由集合。
type API struct {
	es        *es.Client
	tail      *logtail.Tailer
	brand     *BrandState
	version   string
	heartbeat time.Duration
}

// Register 注册全部 API 路由。
func Register(srv *framework.Server, opts Options) {
	hb := opts.HeartbeatInterval
	if hb <= 0 {
		hb = 15 * time.Second
	}
	a := &API{es: opts.ES, tail: opts.Tail, brand: opts.Brand, version: opts.Version, heartbeat: hb}
	g := srv.GET
	g("/api/state", a.handleState)
	g("/api/health", a.handleHealth)
	g("/api/es/root", a.proxy("GET", "/"))
	g("/api/es/health", a.proxy("GET", "/_cluster/health"))
	g("/api/es/stats", a.proxy("GET", "/_cluster/stats"))
	g("/api/es/nodes", a.proxy("GET", "/_nodes/stats"))
	g("/api/es/indices", a.proxy("GET", "/_cat/indices?format=json&bytes=b"))
	g("/api/es/shards", a.proxy("GET", "/_cat/shards?format=json&bytes=b"))
	srv.POST("/api/es/rest", a.handleRest)
	g("/api/logs", a.handleLogs)
	g("/api/logs/stream", a.handleLogStream)
	g("/api/brand", a.handleBrandGet)
	srv.POST("/api/brand", a.handleBrandSet)
	srv.POST("/api/brand/reset", a.handleBrandReset)
}

// ---------------------------------------------------------------------------
// 状态与探活

func (a *API) handleState(c *framework.Context) {
	st := map[string]any{
		"version": a.version,
		"brand":   a.brand.Get(),
		"logs": map[string]string{
			"dir":     a.tail.Dir(),
			"pattern": "*",
		},
		"es": map[string]any{"ok": false},
	}
	if r, err := a.es.Ping(ctxOf(c)); err == nil && r.StatusCode == 200 {
		var root map[string]any
		if jsonx.Unmarshal(r.Body, &root) == nil {
			st["es"] = map[string]any{
				"ok":          true,
				"name":        root["name"],
				"clusterName": root["cluster_name"],
				"uuid":        root["cluster_uuid"],
				"version":     root["version"],
			}
		}
	}
	c.JSON(200, st)
}

func (a *API) handleHealth(c *framework.Context) {
	r, err := a.es.Health(ctxOf(c))
	switch {
	case err != nil:
		c.JSON(200, map[string]any{"ok": false, "error": err.Error()})
	case r.StatusCode != 200:
		c.JSON(200, map[string]any{"ok": false, "status": r.StatusCode, "error": string(r.Body)})
	default:
		var h map[string]any
		_ = jsonx.Unmarshal(r.Body, &h)
		c.JSON(200, map[string]any{"ok": true, "health": h})
	}
}

// ---------------------------------------------------------------------------
// ES 透传

// proxy 生成 GET 类透传处理器。
func (a *API) proxy(method, path string) framework.HandlerFunc {
	return func(c *framework.Context) {
		r, err := a.es.Do(ctxOf(c), method, path, nil)
		a.respondES(c, r, err)
	}
}

func (a *API) handleRest(c *framework.Context) {
	var req struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	method := req.Method
	if method == "" {
		method = "GET"
	}
	path := req.Path
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	var body []byte
	if len(req.Body) > 0 {
		body = req.Body
	}
	r, err := a.es.Do(ctxOf(c), method, path, body)
	a.respondES(c, r, err)
}

// respondES 统一回传 ES 原始响应（状态码透传；body 原样）。
func (a *API) respondES(c *framework.Context, r *es.Response, err error) {
	if err != nil {
		c.JSON(502, map[string]string{"error": "elasticsearch unreachable: " + err.Error()})
		return
	}
	ct := "application/json; charset=utf-8"
	c.SetRespHeader("X-ES-Status", strconv.Itoa(r.StatusCode))
	c.Data(r.StatusCode, ct, r.Body)
}

// ---------------------------------------------------------------------------
// 日志

func (a *API) handleLogs(c *framework.Context) {
	files, err := a.tail.ListFiles()
	if err != nil {
		c.JSON(200, map[string]any{"ok": false, "error": err.Error(), "files": []logtail.FileInfo{}})
		return
	}
	if files == nil {
		files = []logtail.FileInfo{}
	}
	c.JSON(200, map[string]any{"ok": true, "dir": a.tail.Dir(), "files": files})
}

// handleLogStream SSE 实时日志流。
//
// 事件协议（EventSource 自定义 event）：
//
//	line   每条日志行（data 为 JSON 字符串，防换行注入）
//	ready  历史回放完成，开始实时追踪
//	rotate 日志轮转（truncate/替换），已从头重开
//	error  错误
//	end    流结束
func (a *API) handleLogStream(c *framework.Context) {
	file := c.Query("file")
	if file == "" || !a.tail.MatchPattern(file) {
		c.JSON(400, map[string]string{"error": "invalid or unmatched file name"})
		return
	}
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "300"))
	if lines <= 0 || lines > 5000 {
		lines = 300
	}

	c.SetRespHeader("Content-Type", "text/event-stream")
	c.SetRespHeader("Cache-Control", "no-cache")
	c.SetRespHeader("X-Accel-Buffering", "no")
	c.Status(200)

	send := func(event, data string) bool {
		b, _ := json.Marshal(data)
		_, err := c.WriteString("event: " + event + "\ndata: " + string(b) + "\n\n")
		if err == nil {
			c.Flush()
		}
		return err == nil
	}

	// 1) 历史回放
	back, err := a.tail.ReadLastLines(file, lines)
	if err != nil {
		send("error", err.Error())
		return
	}
	for _, ln := range back {
		if !send("line", ln) {
			return
		}
	}
	if !send("ready", "") {
		return
	}

	// 2) 实时追踪（会话收尾 → ctx 取消；客户端断开由心跳写失败感知——
	// 这是长连接的可靠断开感知机制，不探测连接读方向以免与请求解析竞争）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	// 心跳：定期注释行，写失败即客户端已断开（取消 Follow）
	heartbeatStop := make(chan struct{})
	go func() {
		t := time.NewTicker(a.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if _, err := c.WriteString(": keep-alive\n\n"); err != nil {
					cancel()
					return
				}
				c.Flush()
			case <-heartbeatStop:
				return
			}
		}
	}()
	offset := a.tail.Size(file)
	_ = a.tail.Follow(ctx, file, offset,
		func(batch []string) bool {
			for _, ln := range batch {
				if !send("line", ln) {
					return false
				}
			}
			return true
		},
		func() { send("rotate", "log rotated, reopened from start") },
	)
	close(heartbeatStop)
	cancel()
	send("end", "stream closed")
}

// ---------------------------------------------------------------------------
// 品牌

func (a *API) handleBrandGet(c *framework.Context) {
	c.JSON(200, a.brand.Get())
}

func (a *API) handleBrandSet(c *framework.Context) {
	var b Brand
	if err := c.BindJSON(&b); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	c.JSON(200, a.brand.Set(b))
}

func (a *API) handleBrandReset(c *framework.Context) {
	c.JSON(200, a.brand.Reset())
}

// ctxOf 请求级 context（客户端断开联动取消；单请求总时限由 Client.Timeout 兜底）。
func ctxOf(c *framework.Context) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}
