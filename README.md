# Yummy 应用平台

Yummy 是一个把源码构建成镜像并部署到 k3s 运行的应用平台：React 控制台与公共 API 由
控制面进程提供，构建与部署执行由 worker 承担，构建 Pod 内的一次性 runner 进程调用
BuildKit 完成镜像构建。全部能力打进一个 `bin/platform` 二进制（含嵌入控制台）。

## 架构

```mermaid
flowchart LR
    UI["React 控制台"] -->|"ConnectRPC /api"| CP["控制面<br/>bin/platform serve"]
    CP --> PG[("PostgreSQL<br/>业务事实源")]
    CP --> Redis[("Redis<br/>会话与在线状态")]
    CP --> SRC[("源码归档目录")]
    CP -->|"mTLS 双向流"| W["worker<br/>bin/platform worker run"]
    W --> K3S["k3s 集群"]
    K3S --> BP["构建 Pod<br/>BuildKit runner"]
    K3S --> RT["应用运行时 Deployment"]
```

- **控制面**：登录与会话、工作空间/应用/构建/部署/OAuth 的 Connect API、审计与授权、
  本地 CLI/MCP 入口，全部在一个进程内按模块组织。
- **worker**：以注册身份经 mTLS 长连接接入控制面，负责构建 Job、部署与运行时观察、
  节点指标采集；事件经 journal 保证不丢、重投幂等。
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

compose 使用 PostgreSQL 18 和 Redis 8。PostgreSQL 18 镜像的数据目录与 17 不兼容，如果本机还留着旧版本创建的容器，先执行 `docker compose down -v` 删除旧容器和数据，再重新启动。

## 快速开始（单机开发）

```bash
# 三把独立密钥（Base64 32 字节），master-key 必须随数据库一同备份
openssl rand -base64 32   # SESSION_KEY
openssl rand -base64 32   # CSRF_KEY
openssl rand -base64 32   # MASTER_KEY
mkdir -p var/source

./bin/platform migrate
./bin/platform admin init --username admin
SESSION_KEY=... CSRF_KEY=... MASTER_KEY=... SOURCE_ROOT=var/source \
  ./bin/platform serve --allow-local-http
```

打开 <http://127.0.0.1:8080>，用 `admin init` 创建的账号登录。`--allow-local-http`
只允许回环地址明文访问，生产部署必须 HTTPS（见 `docs/operations.md`）。

## 控制面与 worker 分离

控制面启用 worker 登记（生成 CA 与服务端证书）：

```bash
./bin/platform serve --worker-ca-cert var/worker-ca.crt --worker-ca-key var/worker-ca.key \
  --worker-cert var/worker.crt --worker-key var/worker.key --worker-addr :8443
```

worker 所在节点先登记身份再运行执行内核：

```bash
./bin/platform worker register --server control.example.com:8443 --token <一次性注册码> --state-dir /var/lib/yummy-worker
./bin/platform worker run --state-dir /var/lib/yummy-worker \
  --kube-apiserver https://10.0.0.5:6443 --kube-ca-file ca.crt --kube-token-file token \
  --node-name node-1 --build-namespace build-demo --runtime-namespace prod-demo
```

worker 主动建立长连接，部署在客户网络时通常只需允许它访问控制面的 worker HTTPS 端口。

## CLI 与 MCP

```bash
./bin/platform client auth login        # PAT 登录
./bin/platform client workspace list    # 工作空间与应用操作
./bin/platform mcp --stdio              # 本地 stdio MCP（源码预览 + 已登录桥接业务工具）
```

## 常用配置

同一配置项的读取优先级：**显式命令行参数 > 配置文件 > 环境变量 > 内置默认值**。
配置文件用 `--config`（或 `-c`）指定，未指定时依次查找 `./config.yaml`、`/etc/yummy/config.yaml`；
键名与 flag 一致，完整示例见 [config.example.yaml](config.example.yaml)。

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
| worker run：`--state-dir` `--kube-apiserver` `--kube-ca-file` `--kube-token-file` `--node-name` `--build-namespace` `--runtime-namespace` | 无（必填） | worker 执行内核连接参数 |

## 开发命令

```bash
make test              # Go race 测试 + React 组件测试
make integration-test  # 使用本机 PostgreSQL/Redis 的真实存储集成测试
make build             # 构建前端并生成 bin/platform
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
