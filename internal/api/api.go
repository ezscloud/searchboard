// Package api searchboard HTTP API。
//
// 路由清单（全部挂在 /api/*，前端与静态资源走 /public/* 与 SPA fallback）：
//
//	GET  /api/state        启动状态（版本/品牌/日志目录/ES 概要/集群概要）
//	GET  /api/health       ES 探活（当前集群）
//	GET  /api/es/root      透传 GET /（当前集群；X-Cluster 头可指定）
//	GET  /api/es/health    透传 _cluster/health
//	GET  /api/es/stats     透传 _cluster/stats
//	GET  /api/es/nodes     透传 _nodes/stats
//	GET  /api/es/indices   透传 _cat/indices?format=json&bytes=b
//	GET  /api/es/shards    透传 _cat/shards?format=json&bytes=b
//	POST /api/es/rest      透传任意 REST（body: {method,path,body}）
//	GET  /api/clusters     多集群列表（脱敏）+ 当前集群
//	POST /api/clusters     新增集群
//	PUT  /api/clusters/:name    更新集群（密码空=不修改）
//	DELETE /api/clusters/:name  删除集群（至少保留一个）
//	POST /api/clusters/current  切换当前集群 {name}
//	POST /api/clusters/test     临时连接测试（不落盘）
//	GET  /api/clusters/status   批量健康探测（并行）
//	GET  /api/logs         日志文件列表
//	GET  /api/logs/stream  SSE 实时日志流（?file=&lines=）
//	GET  /api/brand        当前品牌
//	POST /api/brand        运行时更新品牌（内存态；重启回落到 YAML）
//	POST /api/brand/logo   logo 上传（base64）与清除；持久到 public_dir 或运行时 Mem 层
//
// 安全说明：本服务定位为本地/内网管理台，默认无鉴权；如需暴露公网，
// 建议前置反向代理加 BasicAuth/IP 白名单（es 密码只存在于本进程内存与
// 配置文件，不落浏览器）。
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/jsonx"
	"github.com/ezscloud/searchboard/framework/vfs"
	"github.com/ezscloud/searchboard/internal/cluster"
	"github.com/ezscloud/searchboard/internal/es"
	"github.com/ezscloud/searchboard/internal/logtail"
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
	ES       *es.Client
	Clusters *cluster.Manager // 多集群注册表；nil=单客户端模式（测试/兼容）
	Tail     *logtail.Tailer
	Brand    *BrandState
	Version  string

	// PublicDir 品牌资源持久目录（logo 上传写入此处，重启不丢）；空=仅运行时
	PublicDir string
	// Stack VFS 分层栈（顶层须为 Mem 层）；PublicDir 为空时 logo 上传写入此处
	Stack *vfs.Stack

	// HeartbeatInterval SSE 心跳间隔（默认 15s）。心跳注释行写失败即感知
	// 客户端断开——这是长连接断开感知的可靠机制（不探测连接读方向）。
	HeartbeatInterval time.Duration
}

// API 路由集合。
type API struct {
	es        *es.Client
	clusters  *cluster.Manager
	tail      *logtail.Tailer
	brand     *BrandState
	version   string
	heartbeat time.Duration
	publicDir string
	stack     *vfs.Stack
}

// Register 注册全部 API 路由。
func Register(srv *framework.Server, opts Options) {
	hb := opts.HeartbeatInterval
	if hb <= 0 {
		hb = 15 * time.Second
	}
	a := &API{es: opts.ES, clusters: opts.Clusters, tail: opts.Tail, brand: opts.Brand, version: opts.Version,
		heartbeat: hb, publicDir: opts.PublicDir, stack: opts.Stack}
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
	// 多集群管理
	g("/api/clusters", a.handleClusters)
	g("/api/clusters/status", a.handleClustersStatus)
	srv.POST("/api/clusters", a.handleClusterAdd)
	srv.PUT("/api/clusters/:name", a.handleClusterUpdate)
	srv.DELETE("/api/clusters/:name", a.handleClusterDelete)
	srv.POST("/api/clusters/current", a.handleClusterCurrent)
	srv.POST("/api/clusters/test", a.handleClusterTest)
	g("/api/logs", a.handleLogs)
	g("/api/logs/stream", a.handleLogStream)
	g("/api/brand", a.handleBrandGet)
	srv.POST("/api/brand", a.handleBrandSet)
	srv.POST("/api/brand/reset", a.handleBrandReset)
	srv.POST("/api/brand/logo", a.handleBrandLogo)
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
	if a.clusters != nil {
		st["clusters"] = map[string]any{
			"current": a.clusters.Current(),
			"names":   a.clusters.Names(),
			"file":    a.clusters.Path(),
		}
	}
	if cl, err := a.clientOf(c); err == nil {
		if r, err := cl.Ping(ctxOf(c)); err == nil && r.StatusCode == 200 {
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
	}
	c.JSON(200, st)
}

func (a *API) handleHealth(c *framework.Context) {
	cl, cerr := a.clientOf(c)
	if cerr != nil {
		c.JSON(200, map[string]any{"ok": false, "error": cerr.Error()})
		return
	}
	r, err := cl.Health(ctxOf(c))
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

// clientOf 解析请求对应的集群客户端：优先 X-Cluster 头，缺省用当前集群。
// 兼容单客户端模式（测试装配）：未启用多集群时直接返回 a.es。
func (a *API) clientOf(c *framework.Context) (*es.Client, error) {
	if a.clusters == nil {
		return a.es, nil
	}
	return a.clusters.Client(c.Header("X-Cluster"))
}

// proxy 生成 GET 类透传处理器。
func (a *API) proxy(method, path string) framework.HandlerFunc {
	return func(c *framework.Context) {
		cl, err := a.clientOf(c)
		if err != nil {
			c.JSON(404, map[string]string{"error": err.Error()})
			return
		}
		r, err := cl.Do(ctxOf(c), method, path, nil)
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
	cl, cerr := a.clientOf(c)
	if cerr != nil {
		c.JSON(404, map[string]string{"error": cerr.Error()})
		return
	}
	r, err := cl.Do(ctxOf(c), method, path, body)
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
// 多集群管理

func (a *API) handleClusters(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(200, map[string]any{"current": "", "list": []cluster.View{}, "file": ""})
		return
	}
	c.JSON(200, map[string]any{
		"current": a.clusters.Current(),
		"list":    a.clusters.List(),
		"file":    a.clusters.Path(),
	})
}

func (a *API) handleClusterAdd(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(500, map[string]string{"error": "多集群未启用"})
		return
	}
	var p cluster.Pick
	if err := c.BindJSON(&p); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if err := a.clusters.Add(p); err != nil {
		c.JSON(400, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(200, map[string]any{"ok": true, "current": a.clusters.Current()})
}

func (a *API) handleClusterUpdate(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(500, map[string]string{"error": "多集群未启用"})
		return
	}
	var p cluster.Pick
	if err := c.BindJSON(&p); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if err := a.clusters.Update(c.Param("name"), p); err != nil {
		c.JSON(400, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(200, map[string]any{"ok": true})
}

func (a *API) handleClusterDelete(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(500, map[string]string{"error": "多集群未启用"})
		return
	}
	if err := a.clusters.Remove(c.Param("name")); err != nil {
		c.JSON(400, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(200, map[string]any{"ok": true, "current": a.clusters.Current()})
}

func (a *API) handleClusterCurrent(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(500, map[string]string{"error": "多集群未启用"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	if err := a.clusters.SetCurrent(req.Name); err != nil {
		c.JSON(400, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(200, map[string]any{"ok": true, "current": a.clusters.Current()})
}

func (a *API) handleClusterTest(c *framework.Context) {
	var p cluster.Pick
	if err := c.BindJSON(&p); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	info, err := cluster.Test(ctxOf(c), p)
	if err != nil {
		c.JSON(200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(200, info)
}

// handleClustersStatus 并行探测全部集群健康（列表页状态列）。
func (a *API) handleClustersStatus(c *framework.Context) {
	if a.clusters == nil {
		c.JSON(200, map[string]any{"list": []any{}})
		return
	}
	vs := a.clusters.List()
	out := make([]map[string]any, len(vs))
	var wg sync.WaitGroup
	for i, v := range vs {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			row := map[string]any{"name": name}
			info, err := a.clusters.Ping(ctxOf(c), name)
			if err != nil {
				row["ok"] = false
				row["error"] = err.Error()
			} else {
				row["ok"] = info["ok"]
				row["ms"] = info["ms"]
				row["error"] = info["error"]
				row["cluster_name"] = info["cluster_name"]
				row["version"] = info["version"]
			}
			out[i] = row
		}(i, v.Name)
	}
	wg.Wait()
	c.JSON(200, map[string]any{"list": out})
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

// ---------------------------------------------------------------------------
// logo 上传

// logoExtAllow 上传扩展名白名单（前端 tryLogo 探测列表一致）。
var logoExtAllow = map[string]bool{"png": true, "svg": true, "jpg": true, "webp": true}

// logoMaxBytes 原始字节数上限（256KB）。
const logoMaxBytes = 256 << 10

// handleBrandLogo logo 上传与清除。
//
// 请求体：{"name":"logo.png","data":"<base64>"} 上传；
//
//	{"clear":true} 清除已上传 logo（public_dir 或 Mem 层中全部白名单扩展）。
//
// 持久化策略：PublicDir 非空 → 写文件（重启不丢）；否则写 VFS Mem 层（运行时）。
func (a *API) handleBrandLogo(c *framework.Context) {
	var req struct {
		Name  string `json:"name"`
		Data  string `json:"data"`
		Clear bool   `json:"clear"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}

	if req.Clear {
		removed := 0
		if a.publicDir != "" {
			for ext := range logoExtAllow {
				if err := os.Remove(filepath.Join(a.publicDir, "logo."+ext)); err == nil {
					removed++
				}
			}
		}
		if a.stack != nil {
			for ext := range logoExtAllow {
				if err := a.stack.Delete("logo." + ext); err == nil {
					removed++
				}
			}
		}
		c.JSON(200, map[string]any{"ok": true, "removed": removed})
		return
	}

	// 上传
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(req.Name), "."))
	if !logoExtAllow[ext] {
		c.JSON(400, map[string]string{"error": "仅支持 png/svg/jpg/webp"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		c.JSON(400, map[string]string{"error": "data 非 base64: " + err.Error()})
		return
	}
	if len(data) == 0 || len(data) > logoMaxBytes {
		c.JSON(400, map[string]string{"error": "文件大小须在 1B–256KB"})
		return
	}

	name := "logo." + ext
	persisted := false
	if a.publicDir != "" {
		if err := os.WriteFile(filepath.Join(a.publicDir, name), data, 0o644); err != nil {
			c.JSON(500, map[string]string{"error": "写入 public_dir 失败: " + err.Error()})
			return
		}
		persisted = true
	} else {
		if a.stack == nil {
			c.JSON(500, map[string]string{"error": "未配置存储层"})
			return
		}
		if err := a.stack.Put(name, data); err != nil {
			c.JSON(500, map[string]string{"error": "写入运行时层失败: " + err.Error()})
			return
		}
	}
	// 清理其他扩展的旧 logo（避免多格式并存探测混乱）
	for e := range logoExtAllow {
		if e == ext {
			continue
		}
		if a.publicDir != "" {
			_ = os.Remove(filepath.Join(a.publicDir, "logo."+e))
		}
		if a.stack != nil {
			_ = a.stack.Delete("logo." + e)
		}
	}
	c.JSON(200, map[string]any{"ok": true, "logo": "/public/" + name, "persisted": persisted})
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
