# React + Go 多拓扑部署 Demo

这个 Demo 使用同一套 React、Go 和 Protocol Buffers 代码，演示三种运行方式：

1. `platform` 不带子命令，默认 `all`：React、控制面和本地 worker 合并为一个进程；
2. Vite + `platform all`：前端开发服务器与 Go 后端分开运行；
3. `control` + `worker`：控制面和执行节点拆成独立进程，worker 可从客户机器经公网主动连接控制面。

目前只实现安全的内置 `uppercase` 任务，不执行任意 Shell 命令。任务数据以 PostgreSQL 为准，Redis 仅保存带 TTL 的 worker 在线状态。

## 架构

```mermaid
flowchart LR
    UI["React 控制台"] -->|"ConnectRPC Unary /api"| CP["Go 控制面"]
    CP --> PG[("PostgreSQL<br/>任务与状态")]
    CP --> Redis[("Redis<br/>在线状态 TTL")]
    CP --> Bus{"CommandBus"}
    Bus -->|"all 模式：函数调用"| Local["本地 worker runtime"]
    Bus -->|"拆分模式：ConnectRPC 双向流<br/>HTTP/2 + mTLS"| Remote["远程 worker / agent"]
    Local -->|"领域事件"| CP
    Remote -->|"Accepted / Progress / Result"| CP
```

控制面只依赖稳定的 `CommandBus`、repository 和 presence 接口。三种拓扑的差异集中在 `internal/bootstrap` 的装配层，业务代码和协议不分叉。

## 前置条件

- Go 1.26 或更高版本；`go.mod` 指定了 `toolchain go1.27.3`，默认的 `GOTOOLCHAIN=auto` 会自动下载并使用该版本；
- Node.js 22.13+、24 或 26+（推荐 24 LTS）、pnpm 10；
- Docker Compose；
- OpenSSL；
- rsync；
- 仅在重新生成协议代码时需要 `protoc`。

## 首次准备

```bash
pnpm --dir web install --frozen-lockfile
docker compose up -d postgres redis
make certs build
```

开发证书会写入 `var/certs`，只用于本地演示。生产或公网部署应由正式 CA 签发证书，并妥善保护 worker 私钥。

compose 使用 PostgreSQL 18 和 Redis 8。PostgreSQL 18 镜像的数据目录与 17 不兼容，如果本机还留着旧版本创建的容器，先执行 `docker compose down -v` 删除旧容器和演示数据，再重新启动。

## 方式一：单二进制、单进程

```bash
./bin/platform
```

不指定子命令时默认等价于：

```bash
./bin/platform all
```

打开 <http://localhost:8080>。页面、公共 API、控制面和 `local-worker` 都在同一个 Go 进程中。

## 方式二：前后端分离

终端一启动 Go 后端：

```bash
./bin/platform all
```

终端二启动 React 开发服务器：

```bash
pnpm --dir web dev
```

打开 <http://localhost:5173>。Vite 将 `/api` 代理到 `http://localhost:8080`。如后端使用其他地址：

```bash
VITE_CONTROL_PROXY=http://127.0.0.1:19081 pnpm --dir web dev
```

生产环境可以将 `web/dist` 放到 Nginx、对象存储或 CDN，并参考 `deploy/nginx.conf` 将 `/api/` 转发给 Go 后端。`platform` 自带的嵌入页面在这种拓扑下可以不使用。

## 方式三：控制面和 worker 分离

先启动控制面：

```bash
./bin/control
```

再启动独立 worker：

```bash
./bin/worker --worker-id worker-1
```

也可以只分发万能二进制：

```bash
./bin/platform control
./bin/platform worker --worker-id worker-1
```

公共控制台/API 默认监听 `:8080`；只供 worker 使用的 mTLS HTTP/2 端口默认监听 `:8443`。worker 主动建立长连接，因此部署在客户网络时通常只需允许它访问控制面的公网 `8443` 端口。

## 常用配置

| 环境变量 / 参数 | 默认值 | 用途 |
|---|---|---|
| `DATABASE_URL` / `--database-url` | `postgres://demo:demo@localhost:5432/demo?sslmode=disable` | PostgreSQL |
| `REDIS_ADDR` / `--redis-addr` | `localhost:6379` | Redis |
| `PUBLIC_ADDR` / `--public-addr` | `:8080` | 页面与公共 ConnectRPC API |
| `AGENT_ADDR` / `--agent-addr` | `:8443` | worker mTLS 双向流 |
| `CONTROL_URL` / `--control-url` | `https://localhost:8443` | worker 回连地址 |
| `WORKER_ID` / `--worker-id` | `worker-1` | worker ID，必须与客户端证书 CN 一致 |
| `CERT_DIR` / `--cert-dir` | `var/certs` | CA、服务端和 worker 证书目录 |
| `TLS_SERVER_NAME` / `--tls-server-name` | `localhost` | worker 校验的服务端证书名称 |

示例：

```bash
CONTROL_URL=https://control.example.com:8443 \
TLS_SERVER_NAME=control.example.com \
CERT_DIR=/etc/platform/certs \
./bin/worker --worker-id customer-a-worker-1
```

需要先为该 worker 生成或签发 CN 为 `customer-a-worker-1` 的客户端证书，并放置为：

```text
/etc/platform/certs/ca.crt
/etc/platform/certs/customer-a-worker-1.crt
/etc/platform/certs/customer-a-worker-1.key
```

## 开发命令

如 `go mod download` 或 `make build` 访问 `proxy.golang.org` 超时，Makefile 已默认启用国内镜像：

```bash
# 默认：goproxy.cn → proxy.golang.org → direct
make build

# 临时改用其他镜像
GOPROXY=https://goproxy.io,direct make build
```

也可写入 shell 配置：`export GOPROXY=https://goproxy.cn,direct`。

```bash
make test              # Go race 测试 + React 组件测试
make integration-test  # 使用本机 PostgreSQL/Redis 的存储集成测试
make build             # 构建前端并生成三个二进制
make generate          # 重新生成 Go/TypeScript 协议代码
make smoke             # 自动验证三种拓扑
```

`make generate` 默认使用系统 `protoc`；本仓库当前也会识别 `work/protoc/bin/protoc`。生成器由 Go tool 依赖和前端 `protoc-gen-es` 提供。

## 数据检查

```bash
docker compose exec postgres psql -U demo -d demo -c \
  "SELECT id, worker_id, status, output, updated_at FROM commands ORDER BY created_at DESC LIMIT 10;"

docker compose exec redis redis-cli --scan --pattern 'demo:worker:*'
```

## 当前边界与后续 HA

- 当前控制面是单实例；同一个进程内的 registry 保存它所连接的 worker。
- 未来多控制面实例需要给 worker 连接增加 owner/租约，并让任务路由到持有该连接的控制面实例，可使用 PostgreSQL/Redis 协调或引入消息系统。
- PostgreSQL 已是唯一任务事实来源，后续可增加 outbox、重试与幂等键；接入 AI 时也便于使用 `pgvector` 等扩展。
- 当前没有证书签发后台、自动任务重投、用户认证和任意代码执行能力。
