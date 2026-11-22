# 架构总览

两张图帮助快速建立全局认识：**部署架构**回答"进程跑在哪、怎么连"，**代码模块分层**回答"代码怎么归类、依赖朝哪流"。约定与细则见 [AGENTS.md](../AGENTS.md)，运维手册见 [operations.md](operations.md)。

## 一、部署架构

```mermaid
flowchart TB
    subgraph clients["客户端"]
        browser["浏览器：React 控制台<br/>（二进制内嵌，无需单独前端部署）"]
        agent["CLI / Agent<br/>（PAT 认证，按需启动）"]
    end

    subgraph control["控制面进程 bin/yummy-server serve（模块化单体）"]
        public_listener["公共监听 :8080<br/>页面 + /api Connect RPC + /oauth<br/>+ 远端 MCP（启用 OAuth 时）<br/>+ /transfer 源码与产物传输"]
        admin_listener["管理监听 :8082（只绑回环）<br/>/healthz + /metrics（--metrics-pull）<br/>公网不可达"]
        worker_listener["worker 网关 :8443<br/>mTLS 双向流长连接"]
        services["业务域服务<br/>账号 / 空间 / 应用 / 构建 / 部署 / 审计<br/>zap JSON 日志 + OTel SDK"]
        public_listener --> services
        transfer_listener --> services
    end

    subgraph instance_infra["实例基础设施"]
        pg[("PostgreSQL<br/>业务事实源")]
        redis[("Redis<br/>会话 + 在线 TTL")]
        sourcedir[("source-root 目录<br/>源码归档")]
    end

    subgraph worker_node["worker 节点（可多台，可在客户网络）"]
        workerproc["bin/yummy-worker run<br/>journal 保证事件不丢、重投幂等"]
        subgraph k3s["k3s 集群（第一版每实例一个）"]
            buildpod["构建 Pod（按任务拉起）<br/>yummy-runner + rootless BuildKit<br/>命名空间 build-空间名"]
            apprt["应用运行时 Deployment<br/>命名空间 prod-空间名"]
        end
    end

    registry[("输出镜像仓库<br/>（全局管理员统一管理）")]
    traefik["Traefik Ingress（集群内置）"]
    collector["OTel Collector（可选）"]
    prometheus["Prometheus（可选）"]

    browser -->|"HTTPS"| public_listener
    agent -->|"Connect RPC"| public_listener
    services --> pg
    services --> redis
    services --> sourcedir
    workerproc -->|"mTLS 注册（一次性注册码）+ 双向流"| worker_listener
    workerproc -->|"驱动"| buildpod
    workerproc -->|"驱动与观察"| apprt
    buildpod -->|"push 镜像"| registry
    apprt -->|"pull 镜像"| registry
    visitors["终端用户"] -->|"应用名-后缀.应用根域名"| traefik
    traefik --> apprt
    prometheus -->|"pull /metrics"| admin_listener
    services -.->|"OTLP push（--otel-endpoint）"| collector
    workerproc -.->|"OTLP push"| collector
```

部署要点速查：

| 项 | 说明 |
|---|---|
| 常驻进程 | 控制面 ×1 + worker ×N；构建 Pod 按任务拉起、结束即退，不算常驻 |
| 按需客户端 | CLI（`yummy`）——不新增任何常驻服务 |
| 控制面监听 | :8080 页面/API/传输（传输自带 10 分钟截止，业务 30s 超时预算）、:8082 管理面 /healthz 与 /metrics 只绑回环、:8443 worker mTLS |
| 数据三件套 | PostgreSQL（事实源）+ source-root（归档）+ master-key，备份必须成套 |
| 命名约定 | 构建 `build-<空间名>`、运行 `prod-<空间名>`、Deployment `<应用名>-<部署名>`、地址 `<应用名>-<后缀>.<根域名>` |
| 网络边界 | worker 主动外连控制面，客户网络只需放行控制面 8443 入站；运行命名空间只放行同命名空间与 Traefik 入站 |
| 可观测性 | pull：serve `--metrics-pull` 暴露 `/metrics`；push：`--otel-endpoint`（或 `OTEL_EXPORTER_OTLP_ENDPOINT`）推 OTLP，三进程统一 |
| 一次性命令 | `migrate`（带锁迁移）、`admin init`（首位全局管理员）、`yummy-worker register`（一次性注册码登记） |

## 二、代码模块分层

```mermaid
flowchart TB
    subgraph entry["① 入口装配"]
        cmdsrv["cmd/yummy-server<br/>控制面分发件"]
        cmdcli["cmd/yummy<br/>按需客户端分发件"]
        cmdworker["cmd/yummy-worker<br/>宿主机分发件"]
        cmdr["cmd/build-runner<br/>构建 Pod 一次性进程"]
        cpcli["controlplane/cli<br/>serve · migrate · admin"]
        cpsrv["controlplane/server<br/>装配全部域服务与网关"]
        webui["controlplane/webui<br/>嵌入 React SPA"]
    end

    subgraph edge["② 传输与聚合"]
        tr["transport<br/>11 个 Connect handler"]
        wg["transport/workergateway<br/>控制面 mTLS 网关"]
        wc["transport/workerclient · transport/workercli<br/>长连接、命令装配与 journal"]
        ccli["transport/clientcli<br/>CLI 客户端"]
        mcp["bff/mcp<br/>远程 MCP（OAuth，单应用绑定）"]
    end

    subgraph domains["③ 业务领域 internal/domains（15 域）"]
        d1["账号与权限<br/>identity · session · oauth · authorization · audit"]
        d2["平台资源<br/>workspace · application · buildsecret · runtimeconfig"]
        d3["构建与执行<br/>build · deployment · source · inventory · enrollment · qualification"]
    end

    subgraph infra["④ 基础设施 internal/infra"]
        i1["store：GORM + gen DAO + goose 迁移"]
        i2["kube（k3s 适配） · workerexec（执行内核）<br/>buildrunner（构建 Pod 逻辑）"]
        i3["registryclient · sourcearchive · testkit"]
    end

    subgraph common["⑤ 公共库 internal/lib（域无关）"]
        cm1["naming：命名规范校验"]
        cm2["observability：trace · metric · log"]
        cm3["security：密钥盒 / ID / 摘要"]
    end

    contract["契约：api/platform/v1 → gen/go（Go）+ web/src/gen（TS）"]
    webapp["web/ React SPA（Tailwind 4 + shadcn/ui）"]

    cmdsrv --> cpcli
    cmdsrv --> webui
    cmdcli --> ccli
    cmdworker --> wc
    cmdr --> i2
    wc --> i2
    cpcli --> cpsrv
    cpsrv --> mcp
    cpsrv --> tr
    cpsrv --> wg
    tr --> domains
    mcp --> domains
    wg --> domains
    domains --> infra
    tr -.-> contract
    ccli -.-> contract
    wc -.-> contract
    wg -.-> contract
    webapp -.-> contract
    entry -.-> cm2
    edge -.-> cm2
    domains -.-> cm2
    infra -.-> cm2
```

分层规则（详见 [AGENTS.md 后端代码分层与模块化](../AGENTS.md#后端代码分层与模块化)）：

1. **依赖严格单向**：①→②→③→④；公共库⑤不依赖任何业务域，领域不 import 传输与装配细节。
2. **域内高内聚**：模型、数据访问、服务同域组织；域间只通过对方显式导出的接口依赖，禁止跨域直查对方数据表或引用内部 struct。
3. **数据访问收口**：常规 CRUD 走 GORM（gen DAO 按域渗透中），复杂 SQL（CTE、锁、RETURNING）集中在各域的数据访问文件；PG 特有能力一律带【数据库专有】注释并写明 MySQL 替代方案——业务代码不出现 SQL 字符串。
4. **可观测性统一**：三信号 SDK 初始化只在 `internal/lib/observability`；业务侧只取 logger（`LoggerFrom(ctx)` 注入 trace_id）与 meter。
5. **分发边界**：`yummy-server` 嵌入 Web 并提供网关服务端；`yummy-worker` 消费 proto 命令并依赖纯运行配置类型与校验。`make check-binary-deps` 检查三个入口各自的依赖。
