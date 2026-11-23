# framework · 轻量级 HTTP / HTTP2 / WebSocket / RPC / QUIC 网络框架

`github.com/ezscloud/searchboard/framework` 是本仓库内置的网络服务框架：**一套路由，五种协议 + 18 项扩展**，
传输层在 Linux/macOS/*BSD 上跑 [cloudwego/netpoll](https://github.com/cloudwego/netpoll)
（epoll/kqueue 事件驱动），在 Windows 上自动回退 Go 标准库 `net`——
上层协议代码完全无感知。

| 能力 | 说明 | 位置 |
|---|---|---|
| HTTP/1.1 服务端 | 自研编解码（CL 定长 + chunked 流式）、keep-alive、前缀树路由、Next 链中间件、优雅停机 | `framework/` |
| **HTTP/2** | TLS-ALPN 协商 + 明文 h2c（prior-knowledge），委托 golang.org/x/net/http2 提供完整多路复用；h1/h2/h3 同端口共存 | `framework/h2.go` |
| HTTP 客户端 | 与服务端同源编解码，拨号走传输层；**keep-alive 连接池**（按 host 池化复用、空闲 TTL 回收，`DisablePool` 可关） | `framework/client.go`、`client_pool.go` |
| WebSocket | RFC 6455 帧编解码、服务端升级（Hijack）、客户端拨号（ws/wss、自动掩码）、close 握手；**permessage-deflate 扩展**（no_context_takeover，双侧协商） | `framework/ws/` |
| RPC | JSON-RPC 2.0（反射注册，HTTP/WS 双通道，批量+通知），编解码走 sonic | `framework/rpc/` |
| QUIC / HTTP3 | quic-go v0.65 封装，`StdHandler()` 复用同套路由；自签证书一键生成 | `framework/h3/` |
| **VFS 分层渲染** | 前端资源多层覆盖：embed 基座 → 磁盘目录覆盖层 → 内存层（运行时写/删）；品牌 logo 定制即用此机制 | `framework/vfs/` |
| **静态服务** | VFS 驱动的静态资源：MIME/强 ETag+304/Range/index/SPA fallback | `framework/ext/static/` |
| JSON | 全框架统一 sonic v1.15.5（`framework/jsonx` 收敛依赖） | `framework/jsonx/` |
| 扩展中间件 | etag/csrf/secure/gzip/i18n/session/basicauth/keyauth/jwt/limiter(BBR)/loadbalance/cache/reverseproxy/monitor(Prometheus)/pprof/trace/autotls/registry + keystore 共 19 项 | `framework/ext/` |

## 与 Hertz 的对比（诚实口径）

[Hertz](https://github.com/cloudwego/hertz) 是字节跳动的企业级 HTTP 框架，多协议同样是扩展实现（http2/http3 在 hertz-contrib，核心原生仅 HTTP/1.1 + netpoll），且**在 Windows 上同样回退 standard 网络库**（实测日志：`Using network library=standard`）——与 framework 的平台策略一致。

本机基准（1 vCPU 云电脑、回环闭环压测、16 并发×8s×2 轮，数字仅供相对参考）：

| 实现 | QPS | p50 | p99 | 备注 |
|---|---|---|---|---|
| framework/std | ~5,500 | 2.3ms | 11ms | **与标准库 net/http 完全持平**（自研编解码零惩罚） |
| net/http(std) | ~5,500 | 2.3ms | 11ms | 基线 |
| hertz(netpoll) | ~4,500 | 2.9ms | 13ms | 单核环境下 netpoll 劣于其 own standard |
| framework/netpoll | ~3,500 | 3.7ms | 16ms | 同上：事件循环与 session 挤单核 |

结论：
- **framework 的 HTTP/1.1 编解码没有性能惩罚**（std 路径与标准库持平）；
- netpoll 在 1 vCPU 单核上不占优（这不是 netpoll 的主场——它为多核高并发设计，本机仅 1 vCPU）；生产多核环境建议实测；
- Hertz 的优势在企业生态（字节内部万级服务验证、适配器生态、代码生成），framework 的优势在零黑盒、全链路自研可审计、与 envkeeper 哲学一致。

## 扩展目录（framework/ext/）

对标 hertz-contrib 的 18 项扩展，全部零新增第三方依赖（能自研的全部自研）：

| 扩展 | 能力 | 对标 |
|---|---|---|
| `etag` | 协商缓存：Wrap 声明式 + If-None-Match 304 短路 | hertz etag |
| `csrf` | double-submit cookie（HMAC 签名，恒时比较） | hertz csrf |
| `secure` | 安全响应头集（CSP/HSTS/XFO/nosniff）+ Host 白名单 + SSL 重定向 | hertz secure |
| `gzip` | 透明响应压缩：**流式管道支持任意大小**（达到 MinLength 即实时压缩输出），小响应整体压缩，SSE/图片/音视频自动跳过 | hertz gzip |
| `i18n` | 多语言字典 + Accept-Language 质量值协商 | hertz i18n |
| `session` | Cookie 会话，Store 接口可插拔（内置 TTL+LRU 内存实现） | hertz sessions |
| `basicauth` | HTTP Basic 认证（常时比较 + 自定义校验器） | hertz basicauth |
| `keyauth` | API-Key/Bearer/Query 三位置提取 + 常时比较 | hertz keyauth |
| `jwt` | HS256/384/512 签发与校验（exp/nbf/alg 全验，纯标准库） | hertz jwt |
| `limiter` | BBR 式自适应限流（CPU 采样 + 延迟基线 EWMA + 保底） | hertz bbr-limiter |
| `loadbalance` | RR/随机/加权随机/一致性哈希/最小请求 + 健康摸除+半开 | hertz loadbalance |
| `cache` | 接口缓存（可插拔 Store + LRU-TTL 内存实现 + X-Cache） | hertz cache |
| `reverseproxy` | 反向代理（多上游 + 摘除重试 + XFF 追加） | hertz reverseproxy |
| `monitor` | Prometheus 文本格式（计数/仪表/直方图 + /metrics） | hertz monitor-prometheus |
| `pprof` | 运行时剖析端点（net/http/pprof 桥接 + 指定名 profile） | hertz pprof |
| `trace` | W3C traceparent 链路追踪 + Exporter 接口 | hertz tracer/obs-opentelemetry |
| `autotls` | 真 ACME 证书（x/crypto/acme/autocert：HTTP-01 挑战 + 缓存可插拔）+ 到期续签 + 离线自签回退 | hertz autotls |
| `keystore` | 密钥/证书存储抽象：命名条目（证书链/私钥/任意数据/元数据），MemStore + FileStore（原子写 0600），AutocertCache 适配器直插 autotls，穿越防护 | 自研（Java KeyStore 思路的 Go 轻量版） |
| `registry` | 注册发现接口 + 内存实现（TTL 剔除 + Watch） | hertz registry(nacos/etcd/...) |

## 扩展中间件（19 项，见上表）+ 三个核心包

- `framework/vfs`：分层虚拟文件系统（Embed/Dir/Mem 层 + Stack 覆盖查找），searchboard 用它实现"embed 前端基座 + 外部目录覆盖 logo"；
- `framework/ext/static`：静态服务（MIME/ETag/Range/SPA），searchboard 的页面与 /public/* 都走它；
- `framework/client_pool.go`：客户端 keep-alive 连接池（池化复用 + 空闲清扫，HEAD/CL=0 可复用）。

## 平台策略（transport 抽象）

```
┌──────────── HTTP / WebSocket / RPC 协议层（只面向 net.Conn 语义） ────────────┐
│   Listener.Serve(session func(net.Conn) bool)     Dial(network, addr, t)     │
└────────┬───────────────────────────────────────────────────────┬──────────────┘
   ┌─────┴────────────────────┐                    ┌─────────────┴─────────────┐
   │ linux / darwin / *bsd    │                    │ windows 及其它平台         │
   │ netpoll v0.7.5 事件驱动  │                    │ 标准库 net                 │
   └──────────────────────────┘                    └───────────────────────────┘
```

| 平台 | 实现 | 依据 |
|---|---|---|
| Linux | netpoll（epoll，LT） | netpoll 官方支持 |
| macOS / *BSD | netpoll（kqueue） | netpoll 官方支持 |
| Windows | 标准库 net | netpoll v0.7.5 的 Windows 实现是**空桩**（`NewEventLoop`/`CreateListener` 一律返回 nil），运行期不可用 |

- `transport.Options.UseNetpoll=false` 可在 Unix 平台强制标准库；
- `Config.Transport: std` 可在 Server 层强制；`auto`（默认）= 有 netpoll 用 netpoll。

## 快速开始

```go
srv := framework.New(framework.Config{
    Addr:      "127.0.0.1:8080",
    Transport: "auto", // auto=netpoll（仅 unix 生效）/ std
})
srv.Use(framework.Recovery(os.Stderr), framework.Logger(os.Stderr))

srv.GET("/hello/:name", func(c *framework.Context) {
    c.JSON(200, map[string]any{"hello": c.Param("name")})
})

// WebSocket
srv.GET("/ws/echo", func(c *framework.Context) {
    conn, err := ws.Upgrade(c, &ws.UpgradeOptions{ReadLimit: 1 << 20})
    if err != nil { return } // Upgrade 已回写 4xx
    for {
        op, data, err := conn.ReadMessage()
        if err != nil { return }
        _ = conn.WriteMessage(op, data)
    }
})

// JSON-RPC（HTTP + WS 双通道）
r := rpc.NewServer()
_ = r.Register("Math", new(MathService)) // func(ctx, *Args) (*Reply, error)
srv.POST("/rpc", r.Handler())
srv.GET("/ws/rpc", r.WebSocket())

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
defer stop()
_ = srv.Run(ctx) // ctx 取消 → ≤5s 优雅停机
```

客户端（同仓库内即可用）：

```go
cli := framework.NewClient()
resp, _ := cli.Get(ctx, "http://127.0.0.1:8080/hello/framework")
defer resp.Close()
var out struct{ Hello string `json:"hello"` }
_ = resp.JSON(&out)

rc := rpc.NewHTTPClient("http://127.0.0.1:8080/rpc")
var num Number
_ = rc.Call(ctx, "Math.Add", AddArgs{A: 1, B: 2}, &num)
```

QUIC/HTTP3（同一套路由挂到 UDP）：

```go
cert, _ := h3.GenerateSelfSignedCert([]string{"127.0.0.1"})
h3srv := h3.New(h3.Config{
    Addr:      "127.0.0.1:8443",
    TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
}, srv.StdHandler())
go h3srv.Run(ctx)
```

demo（YAML 配置驱动，覆盖全部特性）：

```powershell
go build -o framework-demo.exe ./cmd/framework-demo
.\framework-demo.exe -config .\cmd\framework-demo\framework-demo.yml
# http://127.0.0.1:8080 与 QUIC https://127.0.0.1:8443
```

HTTP/2：TLS 场景默认开启（ALPN 自动协商 h2）；明文 h2c 需显式：

```go
srv := framework.New(framework.Config{
    Addr:       ":8443",
    TLS:        tlsCfg,          // 有 TLS 即启用 h2（ALPN）
    EnableH2C:  true,            // 可选：同时接受明文 h2c
    DisableH2:  false,           // 关闭 h2 时置 true
})
// 客户端（标准库）：
cli := &http.Client{Transport: &http.Transport{
    TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true,
}}
// 客户端（h2c）：
cli2 := &http.Client{Transport: &http2.Transport{
    AllowHTTP: true, DialTLSContext: plainDial,
}}
```

扩展用法示例：

```go
srv.Use(secure.Middleware(secure.Config{HSTSMaxAge: 31536000}))
srv.Use(gzip.Middleware(gzip.Config{MinLength: 1024}))
srv.Use(basicauth.Middleware(basicauth.Accounts{"u": "p"}, basicauth.Config{}))
srv.Use(jwt.Middleware(jwt.NewHS256([]byte(secret), 15*time.Minute)))
srv.Use(session.Middleware(store, "app", nil))
srv.Use(limiter.Middleware(limiter.NewBBR(limiter.Config{})))
srv.Use(cache.Middleware(cache.Config{TTL: 30 * time.Second}))
srv.Use(monitor.Middleware(reg))
srv.Use(trace.Middleware(tracer))
pprof.Mount(srv)                    // /debug/pprof/
srv.GET("/v", etag.Wrap(`"v1"`, h))  // 协商缓存
proxy := reverseproxy.Handler(reverseproxy.Config{Targets: []loadbalance.Endpoint{...}})
```

## 架构要点

1. **session 契约**：`Listener.Serve(session)` 为每条连接在传输层管理的执行流中
   运行 session（std=每连接一个 goroutine；netpoll=OnRequest 任务 goroutine，
   processing 锁保证同一连接不重入）。session 阻塞读完全合法——netpoll 的
   `Connection` 完整实现了 net.Conn（阻塞 Read/Write + Deadline）。
2. **连接移交**：session 返回 `true` 表示连接所有权已移交（WebSocket 升级给
   业务 hub），传输层不再关闭；返回 `false` 由传输层统一关闭。
3. **两段读超时**：等下一请求首字节用 `IdleTimeout`，首字节到达后读头部改用
   `ReadHeaderTimeout`（对齐 net/http 语义，避免空闲连接被头部时限误杀）。
4. **响应写出**：处理器设了 `Content-Length` → 定长；未定长且 ≤2KB → 结束时
   自动定长；超限或主动 `Flush()` → HTTP/1.1 chunked 流式（HEAD 丢弃体但保留
   长度元信息）。
5. **sonic 收敛**：全部 JSON 编解码经 `framework/jsonx`，版本只在 go.mod 出现一次。

## 实测验证记录（2026-11-22，本机）

| 验证项 | 结果 |
|---|---|
| 三平台六架构交叉编译（linux/darwin/windows × amd64/arm64） | 全绿 |
| Windows go1.27.4：`go test ./...`（std 路径，含 searchboard） | 全绿 |
| WSL Ubuntu-24.04：netpoll 真实路径全部测试（framework/ws/rpc/h3/exttest/logtail/boardapi 七包） | 全绿 |
| 标准库 http.Client / http2.Transport 互操作（打自研服务端） | 通过 |
| demo 冒烟 Windows（transport=std）+ WSL（transport=netpoll） | health/hello/RPC/SSE 全 200，QUIC 就绪 |
| searchboard 冒烟（mock ES + 浏览器实测：总览/节点/索引/分片/REST/SSE 日志/品牌定制） | 全通过（含 SSE 实时追加/轮转/颜色） |
| 基准（见「与 Hertz 的对比」节） | framework/std 与标准库持平；netpoll 单核不占优 |

### 踩坑记录（netpoll 集成的硬点）

1. **`io.WriteString` 陷阱**：netpoll `Connection` 实现了 `io.StringWriter`
   （nocopy `WriteString` 只入输出缓冲**不 flush**），`io.WriteString` 会优先
   选它——导致"纯写字符串"的请求永远发不出去（Go 标准库连接无此问题）。
   框架内所有写路径一律 `Write([]byte)`，并在 `writeChunked`/`writeAll` 处注释。
2. **EOF 语义**：netpoll 对端关闭返回自家 `ErrEOF`（syscall.Errno 0x106）而非
   `io.EOF`。`transport.Dial` 统一包一层 `eofConn` 归一化，两条实现语义一致。
3. **h2c 前导嗅探与帧错位**：明文 HTTP/2 需先消耗 24 字节前导再交给
   `http2.Server.ServeConn`，且回放层必须严格只回放 bufio 缓冲存量（多读
   一字节都会让帧解析错位 → FRAME_SIZE_ERROR）。
4. **gzip 后置改写的顺序依赖**：`ReplaceBody` 依赖"未定长"判定，先设
   `Content-Length` 再替换体会被拒绝改写 → 头长与体长不一致 → 帧错位
   （表现为下游 "Unsolicited response"）。顺序：先 ReplaceBody 再设头。
5. **gzip warmup 缓冲归属**（实测 405 事故）：流式 gzip 的 warmup 阶段数据在
   `gzipSink.buf`，原出口缓冲为空——小响应收尾若取 `orig.Snapshot()` 会拿到
   空体（CL=0、body 丢失、keep-alive 下一请求解析错位返回 405）。修复：小响应
   直接压缩/透传 `g.buf` 写回原出口，由 Finish 自动定长。
6. **SSE 断开感知不可探测读方向**：连接上的字节归下一请求解析所有，任何
   "Read 1 字节探活"都会吞掉下一请求首字节（实测把 `GET` 吞成 `ET` → 405 交替
   出现）。长连接断开感知只能用**写失败**（定期写心跳注释行）。`Context.Done()`
   语义为"会话收尾关闭"，长响应处理器配合心跳使用。

## 设计取舍与已知限制

- 客户端连接池按 host 池化（`MaxIdleConnsPerHost` 默认 4，`IdleTimeout` 默认 90s）；
  无池上限硬性协议校验（池内连接被服务端关闭后由复用探测剔除）。
- WS 扩展仅 permessage-deflate（no_context_takeover）；WS over HTTP/3
  （WebTransport）不支持，WS 在 h2 链路不可用（仅 h1）。
- gzip 全尺寸响应可压缩（流式管道）；etag 自动生成仍受缓冲窗口限制
  （Wrap 声明式不受限）。SSE/图片/音视频/Range/HEAD 自动跳过压缩。
- h2 连接停机为**有限优雅**（尽力 GOAWAY）：Shutdown 时对活跃 h2 连接手工发
  `GOAWAY(NO_ERROR, Last-Stream-ID=Max)`，客户端停发新流、现流完成后关闭；
  排空窗口外仍强制断开（x/net/http2 无公开优雅 API，同 Go 标准库
  golang/go#22091）。Hijack 在 h2/h3 链路不可用（多路复用下的连接接管语义
  冲突，net/http 与 Hertz 同样不支持；WS 自动走 h1 ALPN 路径）。
- autotls 内置真 ACME（autocert：HTTP-01 + 缓存可插拔 + 自动续期）；无公网入口
  时自动回退自签。
- SSE 等长响应断开感知依赖心跳写失败（见踩坑 6）；`Context.Done()` 在会话
  收尾时关闭。
- netpoll 路径读背压为"硬上限"模式；协议层由 MaxHeaderBytes/MaxBodyBytes 兜底。
- `-race` 检测需 cgo，交叉编译产物不可用；建议在有 Go 的 Linux 环境补跑。
## 依赖

| 依赖 | 版本 | 用途 |
|---|---|---|
| github.com/cloudwego/netpoll | v0.7.5 | unix 传输层 |
| github.com/bytedance/sonic | v1.15.5 | JSON 编解码（go1.17+<1.28 JIT，其余自动回退） |
| github.com/quic-go/quic-go | v0.65.0 | QUIC/HTTP3（要求 go≥1.26） |
| golang.org/x/net | v0.56.0 | HTTP/2（x/net/http2 委托实现 + h2 测试） |
| gopkg.in/yaml.v3 | v3.0.1 | demo 配置（仓库既有） |

License 同仓库根：MIT。
