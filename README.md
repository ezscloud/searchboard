# searchboard · Elasticsearch / OpenSearch 监控管理台 + 自研 Web 框架

[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](./LICENSE)

单仓库两大组件：

| 组件 | 说明 |
|---|---|
| **searchboard/** | Elasticsearch / OpenSearch 监控管理台（替代 cerebro / elasticvue），特色：与 ES 节点同机部署可直接实时查看节点日志（SSE 流式） |
| **framework/** | 自研 Web 框架：一套路由跑 HTTP/1.1、HTTP/2（TLS-ALPN + h2c）、WebSocket、JSON-RPC 2.0、QUIC/HTTP3 + 18 项扩展中间件 |

另有 **cmd/framework-demo**：框架全特性演示（YAML 配置驱动）。

## 快速开始

```bash
# 构建 ES 监控管理台（二进制输出到 bin/，不污染源码目录）
go build -o bin/searchboard ./searchboard
./bin/searchboard -config ./searchboard/searchboard.yml
# 浏览器打开 http://127.0.0.1:9201

# 框架全特性演示（HTTP/WS/RPC/QUIC）
go build -o bin/framework-demo ./cmd/framework-demo
./bin/framework-demo -config ./cmd/framework-demo/framework-demo.yml
```

## 组件文档

- 框架（架构、18 项扩展、与 Hertz 对比基准、踩坑记录）：[framework/README.md](./framework/README.md)
- 监控管理台（功能、API、配置、安全模型）：[searchboard/README.md](./searchboard/README.md)

## 技术要点速览

- **传输层双策略**：Linux/macOS 走 cloudwego/netpoll（epoll/kqueue 事件驱动）；Windows 上 netpoll 是空桩，自动回退标准库 net，协议层零感知
- **VFS 分层渲染**：embed 前端基座 → 磁盘覆盖层（public_dir 放 logo.png 即可品牌定制）→ 内存层
- **gzip 流式压缩**：任意大小响应实时压缩（warmup/active/passthrough 状态机），SSE/图片自动跳过
- **h2 有限优雅停机**：Shutdown 对活跃 h2 连接发 GOAWAY(NO_ERROR)，现流排空后关闭
- **SSE 断开感知**：心跳写失败感知（不做连接读方向探测——会与下一请求解析竞争）
- **测试**：Windows std 路径 + WSL Linux netpoll 路径全绿；三平台六架构交叉编译全绿

## CI

GitHub Actions：push / PR 触发 `go vet + go test + 交叉编译 + -race`（[.github/workflows/ci.yml](./.github/workflows/ci.yml)）。

## License

MIT — 见 [LICENSE](./LICENSE)
