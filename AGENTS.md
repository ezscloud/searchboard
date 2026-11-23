# AGENTS.md · 本仓库 Agent 操作规范

> 面向对本仓库执行操作的 AI agent / 自动化工具。违反以下约定会破坏构建、
> 行尾一致性或提交历史——**先读此文件，再动手**。

## 1. Shell：Windows 下只允许使用 git-bash

本仓库开发主力机是 Windows，但**一切命令必须通过 git-bash 执行**：

```bash
# 正确：git-bash（位于 D:\Apps\Git\bin\bash.exe）
bash -lc 'go test ./... -p 1'
bash run-all.sh          # 脚本落在 .temp/ 下执行

# 禁止：PowerShell / cmd / WSL bash（C:\Windows\System32\bash.exe）
# - PowerShell 的 Get-Content/正则替换会以 ANSI 误读 UTF-8 中文注释，
#   造成乱码写回（本仓库发生过：strconv→etrconv 级别的字符级损坏）
# - cmd 无管道语义
# - WSL bash 是另一套文件系统视图，/mnt/d 路径访问慢且大小写规则不同
```

细则：

- **长命令**写成 `.sh` 文件放在 `.temp/`（该目录不入库），`bash <脚本>` 执行；
- **引号/转义**按 bash 语法（单引号保字面、双引号可展开）；
- **路径**用 `/d/golang/searchboard` 形式或相对路径，避免 `\` 转义歧义；
- git 操作（commit/push）在 git-bash 下天然无编码问题；
- 需要进程/服务级管理（端口占用排查、taskkill）等 git-bash 做不了的事，
  说明原因后再选其他方式，但**禁止用 PowerShell 做文件内容的读写改**。

## 2. 行尾：LF，已由 .gitattributes 强制

- 仓库根 `.gitattributes`：`* text=auto eol=lf`——**不要修改此文件**；
- git-bash 中新建的 `.sh` 脚本必须保存为 LF（CRLF 脚本 bash 直接报
  `\r: command not found`）；
- 不设置 `core.autocrlf`（仓库级已显式 `false`）。

## 3. GitHub 操作：统一走 gh CLI

gh 已安装（`D:\Apps\gh\bin`，版本 ≥ 2.105）：

```bash
gh pr list / gh pr create          # Pull Request
gh issue list / gh issue create     # Issue
gh release create v0.1.0 --notes-file RELEASE.md
gh api repos/ezscloud/searchboard/...   # 原始 API
gh run list / gh run watch          # CI（GitHub Actions）
gh repo view / gh browse            # 仓库/网页直达
```

- **认证**：`gh auth login`（首次）；状态检查 `gh auth status`；
- **凭证边界**：不在命令行、日志、提交内容中出现 token/密码明文；
- git push 走 SSH（密钥 `D:\Apps\ssh\id_ed25519`，仓库已配 `core.sshCommand`），
  gh 的 API 操作与 git 的 SSH 推送互不依赖。

## 4. 构建 / 测试 / 提交（必须按顺序走完）

```bash
# 1) 构建（产物只进 bin/，禁止输出到根目录——会与源码目录同名冲突）
go build -o bin/searchboard     ./searchboard
go build -o bin/framework-demo  ./cmd/framework-demo

# 2) 静态检查
gofmt -l . && go vet ./...

# 3) 全量测试（-p 1 串行：1 vCPU 机器并发跑测试包会资源竞争假失败）
go test ./... -count=1 -timeout 300s -p 1

# 4) 提交推送（中文 commit message，说明变更与根因）
git add -A && git commit -m "..." && git push
```

提交前检查清单：

- [ ] `gofmt -l .` 输出为空
- [ ] `go vet ./...` 通过
- [ ] `go test ./... -p 1` 全绿（并发偶发失败先单跑复现，确认是资源竞争再串行重跑）
- [ ] `git status` 无意外文件（构建产物只在 `bin/`，`.temp/` 不入库）
- [ ] 新增依赖只在 `go.mod` 出现一次，`go mod tidy` 后无残留

## 5. 环境事实（agent 必须知道）

| 项 | 值 |
|---|---|
| Go | 1.27.x（`go.mod` 要求 1.27；quic-go ≥1.26） |
| 平台 | Windows Server 云电脑，**C 盘会周期重置**，持久数据全在 D 盘 |
| Git | `D:\Apps\Git`（git-bash 同源） |
| gh | `D:\Apps\gh`（PATH 已持久写入 HKCU） |
| SSH key | `D:\Apps\ssh\id_ed25519`（无 passphrase） |
| 仓库 | `D:\golang\searchboard`，module `github.com/ezscloud/searchboard` |
| CPU | 1 vCPU——重任务串行执行；CI 上的 `-race`/多核验证看 GitHub Actions |
| 网络 | github.com 可达但 release 资产下载慢，走 `https://gh-proxy.com/` 前缀镜像并校验 SHA256 |

## 6. 禁止事项（历史事故记录）

1. **PowerShell 读写文件内容**——UTF-8 中文注释会被 ANSI 误读写成乱码；
2. **`.gitignore` 裸名 pattern**（不带 `/`）——曾把源码目录 `searchboard/`
   整个吞掉（同名构建产物的裸名 pattern 匹配任意层级目录）；
3. 修改 `.gitattributes` / `LICENSE`；
4. 构建产物写入根目录；
5. 未经全量测试直接推送。

## 7. 目录速览

```
framework/          自研 Web 框架（HTTP1/2/WS/RPC/QUIC + 18 扩展 + VFS）
searchboard/        ES/OpenSearch 监控管理台（SSE 节点日志实时流）
cmd/framework-demo/ 框架全特性演示
bin/                构建产物（.gitignore）
.temp/              agent 临时文件（.gitignore）
```
