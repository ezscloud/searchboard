# searchboard · Elasticsearch / OpenSearch 监控管理台

轻量单二进制管理台，目标替代 [cerebro](https://github.com/cerebroapp/cerebro) / [elasticvue](https://github.com/cars10/elasticvue)。

**特色能力：与 ES 节点同机部署时，可直接在浏览器实时查看节点日志**（SSE 流式，尾部回放 + 实时追加 + 滚动轮转自动重开 + ERROR/WARN 着色）——这解决的是"ES 出问题时还要 ssh 上去 tail -f"的日常痛点。

基于同仓库 [framework](../framework/README.md) 网络框架构建：单二进制、单文件前端（深色主题，零外部依赖）、YAML 配置、VFS 分层覆盖做品牌定制。

## 功能

| 页面 | 能力 |
|---|---|
| 总览 | 集群健康/节点数/索引数/文档总数/存储用量/分片数卡片；集群信息表；节点资源（堆/磁盘/CPU 条形图） |
| 节点 | `_nodes/stats` 明细：JVM 堆、磁盘、CPU、文档量、写入/查询速率 |
| 索引 | `_cat/indices`：健康徽章、状态、UUID、主副分片、文档数、存储、创建时间，支持过滤 |
| 分片 | `_cat/shards`：主/副、状态筛选（STARTED/INITIALIZING/RELOCATING/UNASSIGNED）、节点分布 |
| REST 控制台 | 任意 REST 透传（method/path/JSON body），响应 pretty 打印 + ES 状态码/耗时/大小 |
| 节点日志 | SSE 实时流：文件选择、回放行数、暂停/清屏、自动滚动、轮转提示 |
| 品牌定制 | 运行时改站点名/标语/logo；`public_dir/logo.png` 放入即生效 |

ES 不可达时前端降级提示（侧边栏红灯 + 总览横幅），10s 轮询自动恢复。

## 快速开始

```powershell
go build -o bin/searchboard.exe ./searchboard
.\bin\searchboard.exe -config .\searchboard\searchboard.yml
# 浏览器打开 http://127.0.0.1:9201
```

## 配置（searchboard.yml，YAML 唯一配置形态）

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

## API

| 端点 | 说明 |
|---|---|
| `GET /api/state` | 启动状态（版本/品牌/日志目录/ES 概要） |
| `GET /api/health` | ES 探活（`_cluster/health` 包装） |
| `GET /api/es/root` `health` `stats` `nodes` `indices` `shards` | ES 端点透传（`X-ES-Status` 头回传真实状态码） |
| `POST /api/es/rest` | 任意 REST 透传：`{method, path, body}` |
| `GET /api/logs` | 日志文件列表（名称/大小/mtime，按修改时间倒序） |
| `GET /api/logs/stream?file=&lines=` | SSE 日志流（事件：`line`/`ready`/`rotate`/`error`/`end`） |
| `GET/POST /api/brand`、`POST /api/brand/reset` | 品牌读取/更新/恢复 YAML 默认 |
| `GET /public/*` | 覆盖层静态资源（品牌 logo） |
| `GET /*` | SPA（embed 基座 + 覆盖层优先，index.html fallback） |

## 架构

```
┌ 浏览器（单文件 index.html，embed 基座）───────────────────────┐
│  侧边栏导航 / 卡片 / 表格 / REST 控制台 / SSE 日志 / 品牌定制    │
└────────────▲──────────────────────────────────────────────────┘
             │ /api/* (JSON/SSE)          /public/* + SPA (静态)
┌────────────┴──────────────────────────────────────────────────┐
│ searchboard（framework 服务端）                                    │
│  ├─ gzip 中间件（流式压缩，SSE 自动跳过）                       │
│  ├─ api 路由（es 透传 / logtail / brand）                       │
│  ├─ es 客户端（framework.Client 连接池 + BasicAuth）               │
│  ├─ logtail（轮询 tail + 轮转检测 + 行缓冲）                    │
│  └─ VFS 栈：public_dir 覆盖层 → embed 基座                     │
└────────────▲──────────────────────────────────────────────────┘
             │ HTTP（连接池 keep-alive）
        Elasticsearch / OpenSearch（任意版本 6.x/7.x/8.x）
```

日志追踪细节：
- 轮询 stat 文件大小（跨平台一致，Windows 无 inotify）；size 减小/文件替换 → 轮转重开从头读；
- 只下发完整行（残行缓冲），单批 ≤2000 行防突发撑爆；
- SSE 心跳（15s 注释行）：写失败即感知客户端断开——**不做连接读方向探测**（会与下一请求解析竞争，详见 framework README 踩坑 6）。

## 安全

- 默认仅监听 `127.0.0.1`（零面暴露）；ES 密码只存在于本进程内存与配置文件，不落浏览器；
- 如需跨机访问：改 `listen` 为内网地址 + 前置反代加 BasicAuth/IP 白名单。

## 实测验证（2026-11-22）

- `go test ./searchboard/...`：logtail 8 项单测（match/列表/回放/大文件/Follow/轮转/防穿越）+ API e2e 8 项（mock ES：state/health/透传/REST/brand 生命周期/SSE 全事件/文件列表/非法文件名拒绝）全绿；
- WSL netpoll 链路全绿；三平台六架构交叉编译全绿；
- 浏览器冒烟（mock ES）：总览/节点/索引/分片/REST/SSE 日志（回放+实时+颜色）/品牌定制全通过。

License 同仓库根：MIT。