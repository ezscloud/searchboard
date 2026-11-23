# searchboard · Elasticsearch / OpenSearch 监控管理台 + 自研 Web 框架

[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/ezscloud/searchboard/actions/workflows/ci.yml/badge.svg)](https://github.com/ezscloud/searchboard/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-MIT-green)](./LICENSE)

单仓库两大组件：

| 组件 | 位置 | 说明 |
|---|---|---|
| **searchboard** | 仓库根（main 包） | Elasticsearch / OpenSearch 监控管理台（替代 cerebro / elasticvue），特色：与 ES 节点同机部署可直接实时查看节点日志（SSE 流式） |
| **framework** | `framework/` | 自研 Web 框架：一套路由跑 HTTP/1.1、HTTP/2（TLS-ALPN + h2c）、WebSocket、JSON-RPC 2.0、QUIC/HTTP3 + 18 项扩展中间件 |

另有 **cmd/framework-demo**：框架全特性演示（YAML 配置驱动）。

## 快速开始

```bash
# 构建 ES 监控管理台（module 根即 main 包）
go build -o bin/searchboard .
./bin/searchboard -config searchboard.yml
# 浏览器打开 http://127.0.0.1:9201

# 框架全特性演示（HTTP/WS/RPC/QUIC）
go build -o bin/framework-demo ./cmd/framework-demo
./bin/framework-demo -config ./cmd/framework-demo/framework-demo.yml
```

## searchboard 功能

| 页面 | 能力 |
|---|---|
| 总览 | 集群健康/节点数/索引数/文档总数/存储用量/分片数卡片；集群信息表；节点资源（堆/磁盘/CPU 条形图） |
| 节点 | `_nodes/stats` 明细：JVM 堆、磁盘、CPU、文档量、写入/查询速率 |
| 索引 | `_cat/indices`：健康徽章、状态、UUID、主副分片、文档数、存储、创建时间，支持过滤；**行内操作：删除（确认弹窗）/ 关闭 / 打开** |
| 分片 | `_cat/shards`：主/副、状态筛选（STARTED/INITIALIZING/RELOCATING/UNASSIGNED）、节点分布 |
| **别名** | `_cat/aliases`：列表/过滤/移除/新增（弹窗表单），指向索引与写别名标记 |
| **集群运维** | 分片均衡开关、并发恢复数（transient 设置下发）；**节点排水/恢复分配**（allocation exclude，状态实时反映） |
| REST 控制台 | 任意 REST 透传（method/path/JSON body），响应 pretty 打印 + ES 状态码/耗时/大小 |
| 节点日志 | SSE 实时流：尾部回放 + 实时追加 + 滚动轮转自动重开 + ERROR/WARN 着色 |
| 品牌定制 | 运行时改站点名/标语/logo；**界面直接上传 logo**（≤256KB，png/svg/jpg/webp；持久到 public_dir 或运行时 VFS 层）+ 清除 |

ES 不可达时前端降级提示（侧边栏红灯 + 总览横幅），10s 轮询自动恢复。

## searchboard 配置（searchboard.yml，YAML 唯一配置形态）

```yaml
listen: 127.0.0.1:9201
transport: auto            # auto/netpoll/std（framework 传输层策略）

elasticsearch:
  url: http://127.0.0.1:9200
  username: ""             # 安全集群填写
  password: ""
  insecure_skip_verify: false

logs:
  dir: ""                  # 日志目录（如 D:/Apps/elasticsearch/logs 或 /var/log/elasticsearch）
  pattern: "*.log"         # 文件名 glob（只匹配单级文件名，防目录穿越）
  poll_interval: 500ms
  max_replay_lines: 300
  max_files: 200

brand:
  name: SearchBoard
  slogan: Elasticsearch 监控管理台
  logo: ""                 # 空=自动探测 /public/logo.{png,svg,jpg,webp}

public_dir: "public"       # 外部覆盖目录（logo 等静态资源覆盖 embed 基座）
```

## searchboard API

| 端点 | 说明 |
|---|---|
| `GET /api/state` | 启动状态（版本/品牌/日志目录/ES 概要） |
| `GET /api/health` | ES 探活（`_cluster/health` 包装） |
| `GET /api/es/root` `health` `stats` `nodes` `indices` `shards` | ES 端点透传（`X-ES-Status` 头回传真实状态码） |
| `POST /api/es/rest` | 任意 REST 透传：`{method, path, body}` |
| `GET /api/logs` | 日志文件列表（名称/大小/mtime，按修改时间倒序） |
| `GET /api/logs/stream?file=&lines=` | SSE 日志流（事件：`line`/`ready`/`rotate`/`error`/`end`） |
| `GET/POST /api/brand`、`POST /api/brand/reset` | 品牌读取/更新/恢复 YAML 默认 |
| `POST /api/brand/logo` | logo 上传（base64，白名单 png/svg/jpg/webp，≤256KB，持久 public_dir 或运行时 VFS 层）与清除 |
| `GET /public/*` | 覆盖层静态资源（品牌 logo） |
| `GET /*` | SPA（embed 基座 + 覆盖层优先，index.html fallback） |

## searchboard 架构

```
┌ 浏览器（单文件 index.html，embed 基座）───────────────────────┐
│  侧边栏导航 / 卡片 / 表格 / REST 控制台 / SSE 日志 / 品牌定制    │
└────────────▲──────────────────────────────────────────────────┘
             │ /api/* (JSON/SSE)          /public/* + SPA (静态)
┌────────────┴──────────────────────────────────────────────────┐
│ searchboard（framework 服务端）                                │
│  ├─ gzip 中间件（流式压缩，SSE 自动跳过）                       │
│  ├─ api 路由（es 透传 / logtail / brand）                      │
│  ├─ es 客户端（framework.Client 连接池 + BasicAuth）           │
│  ├─ logtail（轮询 tail + 轮转检测 + 行缓冲）                   │
│  └─ VFS 栈：public_dir 覆盖层 → embed 基座                     │
└────────────▲──────────────────────────────────────────────────┘
             │ HTTP（连接池 keep-alive）
        Elasticsearch / OpenSearch（任意版本 6.x/7.x/8.x）
```

日志追踪细节：
- 轮询 stat 文件大小（跨平台一致，Windows 无 inotify）；size 减小/文件替换 → 轮转重开从头读；
- 只下发完整行（残行缓冲），单批 ≤2000 行防突发撑爆；
- SSE 心跳（15s 注释行）：写失败即感知客户端断开——**不做连接读方向探测**（会与下一请求解析竞争，详见 framework README 踩坑 6）。

危险操作防护：索引删除/别名移除/节点排水均经确认弹窗（危险操作红色按钮），全部经 `/api/es/rest` 透传（无隐藏通道，可审计）。

## 安全

- 默认仅监听 `127.0.0.1`（零面暴露）；ES 密码只存在于本进程内存与配置文件，不落浏览器；
- 如需跨机访问：改 `listen` 为内网地址 + 前置反代加 BasicAuth/IP 白名单。

## framework 组件文档

架构、18 项扩展、keystore 密钥存储、与 Hertz 对比基准、踩坑记录：
[framework/README.md](./framework/README.md)

## 测试与 CI

- Windows 本地：`go test ./... -p 1`（8 包全绿）；WSL Ubuntu-24.04：Linux netpoll 路径全绿
- 三平台六架构交叉编译全绿
- GitHub Actions：push/PR 触发 `go vet + go test + -race + 交叉编译`（[.github/workflows/ci.yml](./.github/workflows/ci.yml)）

## License

MIT — 见 [LICENSE](./LICENSE)
