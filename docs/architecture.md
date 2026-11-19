# 架构总览

两张图帮助快速建立全局认识：**部署架构**回答"进程跑在哪、怎么连"，**代码模块分层**回答"代码怎么归类、依赖朝哪流"。约定与细则见 [AGENTS.md](../AGENTS.md)，运维手册见 [operations.md](operations.md)。

## 一、部署架构

```mermaid
flowchart TB
    subgraph clients["客户端"]
        browser["浏览器：React 控制台<br/>（二进制内嵌，无需单独前端部署）"]
        agent["CLI / 本地 stdio MCP / Agent<br/>（PAT 认证，按需启动）"]
    end

    subgraph control["控制面进程 bin/platform serve（模块化单体）"]
        public_listener["公共监听 :8080<br/>页面 + /api Connect RPC + /oauth<br/>+ 远端 MCP（启用 OAuth 时）<br/>+ /metrics（--metrics-pull）"]
        transfer_listener["传输监听 :8081<br/>源码归档上传（独立超时预算）"]
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
        workerproc["bin/platform worker run<br/>journal 保证事件不丢、重投幂等"]
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
    prometheus -->|"pull /metrics"| public_listener
    services -.->|"OTLP push（--otel-endpoint）"| collector
    workerproc -.->|"OTLP push"| collector
```

部署要点速查：

| 项 | 说明 |
|---|---|
| 常驻进程 | 控制面 ×1 + worker ×N；构建 Pod 按任务拉起、结束即退，不算常驻 |
| 按需客户端 | CLI（`platform client`）、本地 MCP（`platform mcp --stdio`）——不新增任何常驻服务 |
| 控制面监听 | :8080 页面与公共 API、:8081 文件传输（独立超时预算）、:8443 worker mTLS |
| 数据三件套 | PostgreSQL（事实源）+ source-root（归档）+ master-key，备份必须成套 |
| 命名约定 | 构建 `build-<空间名>`、运行 `prod-<空间名>`、Deployment `<应用名>-<部署名>`、地址 `<应用名>-<后缀>.<根域名>` |
| 网络边界 | worker 主动外连控制面，客户网络只需放行控制面 8443 入站；运行命名空间只放行同命名空间与 Traefik 入站 |
| 可观测性 | pull：serve `--metrics-pull` 暴露 `/metrics`；push：`--otel-endpoint`（或 `OTEL_EXPORTER_OTLP_ENDPOINT`）推 OTLP，三进程统一 |
| 一次性命令 | `migrate`（带锁迁移）、`admin init`（首位全局管理员）、`worker register`（一次性注册码登记） |

## 二、代码模块分层

```mermaid
flowchart TB
    subgraph entry["① 入口装配"]
        cmdp["cmd/platform<br/>唯一常驻入口二进制"]
        cmdr["cmd/build-runner<br/>构建 Pod 一次性进程"]
        cpcli["controlplane/cli<br/>serve · worker · migrate · admin · mcp · client"]
        cpsrv["controlplane/server<br/>装配全部域服务与网关"]
        webui["controlplane/webui<br/>嵌入 React SPA"]
    end

    subgraph edge["② 传输与聚合"]
        tr["transport<br/>11 个 Connect handler"]
        wg["transport/workergateway<br/>worker mTLS 网关 + 事件 journal"]
        ccli["transport/clientcli<br/>CLI 客户端"]
        mcp["bff/mcp<br/>MCP 桥（跨域聚合）"]
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

    cmdp --> cpcli
    cmdp --> cpsrv
    cmdr --> i2
    cpcli --> cpsrv
    cpcli --> ccli
    cpcli --> mcp
    cpsrv --> tr
    cpsrv --> wg
    cpsrv --> webui
    tr --> domains
    mcp --> domains
    wg --> domains
    domains --> infra
    tr -.-> contract
    ccli -.-> contract
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
5. **已知例外**：部署链的 kube / workerexec 共用 `runtimeconfig` 视图类型（控制面物化 → worker 消费的契约），后续以 proto 契约收敛，收敛前维持单向依赖不做反向扩散。
