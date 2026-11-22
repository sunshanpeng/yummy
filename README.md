# Yummy 应用平台

Yummy 是一个把源码构建成镜像并部署到 k3s 运行的应用平台：React 控制台与公共 API 由
控制面进程提供，构建与部署执行由 worker 承担，构建 Pod 内的一次性 runner 进程调用
BuildKit 完成镜像构建。控制面、执行宿主机和用户客户端各有独立的分发二进制。

## 架构

```mermaid
flowchart LR
    UI["React 控制台"] -->|"ConnectRPC /api"| CP["控制面<br/>bin/yummy-server serve"]
    CP --> PG[("PostgreSQL<br/>业务事实源")]
    CP --> Redis[("Redis<br/>会话与在线状态")]
    CP --> SRC[("源码归档目录")]
    CP -->|"mTLS 双向流"| W["worker<br/>bin/yummy-worker run"]
    W --> K3S["k3s 集群"]
    K3S --> BP["构建 Pod<br/>BuildKit runner"]
    K3S --> RT["应用运行时 Deployment"]
```

- **控制面**：`yummy-server` 嵌入 Web 控制台并提供 Connect API、远程 MCP、登录与授权、调度与审计。
- **worker**：以注册身份经 mTLS 长连接接入控制面，负责构建 Job、部署与运行时观察、
  节点指标采集；事件经 journal 保证不丢、重投幂等。
- **CLI**：`yummy` 是按需启动的用户客户端，通过公开 API 操作应用、构建和部署。
- **构建 runner**：`cmd/build-runner` 只由构建 Pod 启动，不是常驻服务。

## 前置条件

- Go 1.26 或更高版本；`go.mod` 指定了 `toolchain go1.27.3`，默认的 `GOTOOLCHAIN=auto` 会自动下载并使用该版本；
- Node.js 22.13+、24 或 26+（推荐 24 LTS）、pnpm 10；
- Docker Compose；
- 仅在重新生成协议代码时需要 `protoc`。

## 首次准备

```bash
pnpm --dir web install --frozen-lockfile
docker compose up -d postgres redis
make build
```

`make build` 构建全部三个二进制。只安装某一端时，可以单独构建对应产物。

| 构建目标 | 产物 | 构建要求 |
|---|---|---|
| `make build-server` | `bin/yummy-server` | Go + Node.js + pnpm |
| `make build-cli` | `bin/yummy` | Go |
| `make build-worker` | `bin/yummy-worker` | Go |

从 macOS 为 Linux/AMD64 的执行宿主机构建时，执行 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build-worker`。
分发时按同一 Git 版本构建各端，并选择与安装机器匹配的平台和架构。

compose 使用 PostgreSQL 18 和 Redis 8。PostgreSQL 18 镜像的数据目录与 17 不兼容，如果本机还留着旧版本创建的容器，先执行 `docker compose down -v` 删除旧容器和数据，再重新启动。

## 快速开始（单机开发）

```bash
# 三把独立密钥（Base64 32 字节），master-key 必须随数据库一同备份
openssl rand -base64 32   # SESSION_KEY
openssl rand -base64 32   # CSRF_KEY
openssl rand -base64 32   # MASTER_KEY
mkdir -p var/source

./bin/yummy-server migrate
./bin/yummy-server admin init --username admin
SESSION_KEY=... CSRF_KEY=... MASTER_KEY=... SOURCE_ROOT=var/source \
  ./bin/yummy-server serve --allow-local-http
```

打开 <http://127.0.0.1:8080>，用 `admin init` 创建的账号登录。`--allow-local-http`
只允许回环地址明文访问，生产部署必须 HTTPS（见 `docs/operations.md`）。

## 控制面与 worker 分离

控制面启用 worker 登记（生成 CA 与服务端证书）：

```bash
./bin/yummy-server serve --worker-ca-cert var/worker-ca.crt --worker-ca-key var/worker-ca.key \
  --worker-cert var/worker.crt --worker-key var/worker.key --worker-addr :8443
```

worker 所在节点先登记身份再运行执行内核。使用自建 CA 时，将控制面 worker HTTPS 证书的签发 CA 证书复制到执行宿主机。

```bash
./bin/yummy-worker register --server https://control.example.com:8443 --token '一次性注册码' \
  --state-dir /var/lib/yummy-worker --server-ca-file /etc/yummy/control-server-ca.crt
./bin/yummy-worker run --state-dir /var/lib/yummy-worker \
  --kube-apiserver https://10.0.0.5:6443 --kube-ca-file ca.crt --kube-token-file token \
  --node-name node-1 --build-namespace build-demo --runtime-namespace prod-demo
```

worker 主动建立长连接，部署在客户网络时通常只需允许它访问控制面的 worker HTTPS 端口。

## CLI 与 MCP

```bash
./bin/yummy auth login --server https://control.example.com
./bin/yummy workspace list
```

`auth login` 使用浏览器 OAuth。使用个人访问令牌时，通过 `yummy auth token --server https://control.example.com --token-stdin` 从标准输入保存 PAT。
Agent 也可通过控制面的远程 Streamable HTTP MCP 接入。认证和代理配置见[运维手册](docs/operations.md)。

## 常用配置

控制面配置与 worker 的全局日志、遥测选项遵循 **显式命令行参数 > 配置文件 > 环境变量 > 内置默认值** 的优先级。
配置文件用 `--config`（或 `-c`）指定，未指定时查找工作目录下的 `config.yaml`、`config.yml`，再查找 `/etc/yummy/config.yaml`。
键名与 flag 一致，示例见 [config.example.yaml](config.example.yaml)。

| 参数（环境变量为对应大写下划线形式） | 默认值 | 用途 |
|---|---|---|
| `--database-url` / `DATABASE_URL` | `postgres://demo:demo@localhost:5432/demo?sslmode=disable` | PostgreSQL |
| `--redis-addr` / `REDIS_ADDR` | `localhost:6379` | Redis；支持 `redis://` / `rediss://` URL |
| `--public-addr` / `PUBLIC_ADDR` | `127.0.0.1:8080` | 页面、ConnectRPC API 与 /transfer 大文件传输 |
| `--admin-addr` / `ADMIN_ADDR` | `127.0.0.1:8082` | 管理监听：/healthz 与 /metrics，只绑回环，公网不可达 |
| `--session-key` / `--csrf-key` / `--master-key` | 无（必填） | 三把独立 Base64 32 字节密钥 |
| `--source-root` / `SOURCE_ROOT` | 无（必填） | 源码归档持久目录，必须预先创建 |
| `--public-base-url` | 无 | 明确的公开站点根 URL，不从请求头推断 |
| `--otel-endpoint` / `OTEL_EXPORTER_OTLP_ENDPOINT` | 空 | OTLP push 端点（trace/metric/log），留空关闭；serve 另有 `--metrics-pull`（管理端口） 暴露 `/metrics` |
| `--log-level` / `LOG_LEVEL` | `info` | 日志级别 debug/info/warn/error |
| `--worker-ca-cert` / `--worker-ca-key` | 空 | worker 签发 CA，留空禁用登记 |
| `--worker-cert` / `--worker-key` / `--worker-addr` | `127.0.0.1:8443` | worker HTTPS 监听器 |

`yummy-worker run` 的 `--state-dir`、`--kube-apiserver`、`--kube-ca-file`、`--kube-token-file`、
`--node-name`、`--build-namespace` 和 `--runtime-namespace` 需要在命令行显式传入。

## 开发命令

```bash
make test              # Go race 测试 + React 组件测试
make integration-test  # 使用本机 PostgreSQL/Redis 的真实存储集成测试
make build             # 构建三个分发二进制
make check-binary-deps # 检查三个入口的依赖边界
make web-build         # 仅构建前端并同步到 Go 嵌入目录
make generate          # 重新生成 Go/TypeScript 协议代码
make smoke             # 产品冒烟：迁移/管理员/登录/建空间/OAuth 元数据最小真实链路
```

`make generate` 默认使用系统 `protoc`；本仓库当前也会识别 `work/protoc/bin/protoc`。生成器由 Go tool 依赖和前端 `protoc-gen-es` 提供。

## 数据检查

```bash
docker compose exec postgres psql -U demo -d demo -c '\dt'
docker compose exec redis redis-cli --scan --pattern 'session:*'
```

## 文档

- 架构总览（部署架构图 + 代码模块分层图）：[docs/architecture.md](docs/architecture.md)
- 部署与运维：[docs/operations.md](docs/operations.md)
- 设计文档：[docs/superpowers/specs/](docs/superpowers/specs/)
- 项目约定：[AGENTS.md](AGENTS.md)
