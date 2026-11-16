# Yummy 第一期运维手册

面向自部署与独立托管实例的安装、配置、升级与限制说明。产品只含控制面（`platform serve`）与 worker（`platform worker run`）两类常驻进程；CLI、本地 stdio MCP 均为按需启动的客户端。PostgreSQL、Redis、Kubernetes（k3s）、镜像仓库为既有基础设施，本产品不额外引入服务。

## 1. 构建与依赖

- 工具链：Go（go.mod 声明版本，工具链 1.27.3 验证）、Node（^22/^24/≥26，pnpm 10）、make、openssl（证书）、docker（本地基础设施，可选）。
- 构建：`make build`（先 `pnpm --dir web install`）。产物：`bin/platform`（控制面 + 管理命令）、`bin/control`、`bin/worker`（旧 demo 拓扑，显式开发模式）。
- 数据库迁移：goose 版本化迁移，进程内带锁执行；`platform migrate --database-url …`。禁止 AutoMigrate。

## 2. 控制面安装与启动

1. 准备 PostgreSQL 与 Redis（Redis 仅承担会话与在线 TTL）。
2. `platform migrate --database-url "postgres://user:pass@host:5432/db?sslmode=disable"`。
3. 初始化首位全局管理员（无匿名初始化接口）：
   `platform admin init --database-url … --username <管理员> --password-stdin`
4. 生成三把独立密钥（Base64 32 字节）：`openssl rand -base64 32`，分别作为 session-key / csrf-key / master-key；如需 OAuth 与远程 MCP 再加 oauth-key。
5. 创建源码持久化目录（`--source-root`，必须预先创建，产品校验可写与 fsync）。
6. 启动：`platform serve`。关键参数（均有同名环境变量，密钥推荐走环境变量）：

| 参数 | 说明 |
|---|---|
| `--database-url` | PostgreSQL 连接地址 |
| `--redis-addr` | Redis 地址或 redis(s) URL |
| `--public-addr` | HTTP 监听地址（默认 127.0.0.1:8080） |
| `--transfer-addr` | 源码传输入口（默认 127.0.0.1:8081，见 §4 反向代理契约） |
| `--public-base-url` | 明确的公开站点根 URL，不从请求头推断 |
| `--session-key` / `--csrf-key` / `--master-key` | 三把独立密钥；master-key 丢失则已加密的源码授权令牌与仓库凭据不可解密，必须随数据库一同备份 |
| `--oauth-key` | 留空关闭 OAuth 与远程 MCP |
| `--source-root` | 源码归档持久目录 |
| `--worker-ca-cert` / `--worker-ca-key` | worker 登记签发 CA；留空禁用登记与 worker 网关 |
| `--worker-cert` / `--worker-key` / `--worker-addr` | worker mTLS 监听的服务端证书与地址（默认 127.0.0.1:8443） |
| `--allow-local-http` | 仅开发：允许回环 HTTP；生产必须 HTTPS |

产品冒烟：`make smoke`（临时库自动清理，验证迁移→管理员→启动→登录→建空间→OAuth 元数据）。

## 3. worker 登记与运行

1. 控制面启用 worker CA（见上）。
2. 全局管理员创建执行节点并签发一次性注册码（全局管理或 `InventoryService.CreateNode` + `EnrollmentService.CreateNodeRegistration`）。
3. worker 机上：`platform worker register --server https://控制面:8443 --token <一次性码> --state-dir /var/lib/yummy-worker --server-ca-file <控制面CA>`。状态目录 0700（私钥/证书/CA/身份），注册码一次性、不可重放。
4. 安装资格：全局管理员先在控制面登记节点验收记录（`RegisterNodeQualification`，含 cluster_node_uid/OS/arch/能力位与验收证据）；worker 以 `--qualification-file` 提供同一记录，控制面逐项核对一致才置能力就绪。任何不一致或缺失即失败关闭——不自报即信。
5. 运行：`platform worker run`（显式 `--kube-apiserver/--kube-ca-file/--kube-token-file/--node-name/--build-namespace/--runtime-namespace`，可选 `--storage-class/--ingress-class/--runner-image/--buildkit-image`——镜像必须 digest 钉住；`--qualification-file` 缺失时全部能力关闭；`--journal-dir` 持久化事件跨重连）。绝不读取 kubeconfig 或环境默认集群。
6. 证书轮换：7 天轮换窗（`/worker/rotate` 自动）；轮换与撤销不影响在途构建下载（节点粒度信任），节点全部证书撤销才阻断。

## 4. 反向代理与 HTTPS（生产必须）

- 公网入口必须由可信反向代理以 HTTPS 终止，并把 **`/transfer/` 前缀转发到 `--transfer-addr` 且保留原始 Host 头**（传输入口校验 Host 必须等于 `--public-base-url` host，防 DNS 重绑定）。未转发该前缀则源码上传在浏览器端必然 404。
- 其余路径（`/`、`/api/`、`/oauth/`、`/.well-known/`）转发到 `--public-addr`。
- 应用自定义域名 HTTPS：管理员预置 TLS Secret 并通过 `SetDomainTLSSecret` 绑定到域名声明；发布领取时全部域名绑定同一非空 Secret 才下发 HTTPS，否则失败关闭。Traefik/ACME 编排属安装侧职责。
- 本地开发可用 `--allow-local-http`，但传输入口仍需端口可达（或按 §2 配置后用 e2e 同款转发）。

## 5. 运行配置与数据保留

- 登录频控：20 次/15 分钟/账号与来源 IP（Redis `yummy:login:*`）。自动化测试连续全量运行前需清理该前缀键或等待窗口。
- 源码保留：快照与上传各自有到期时间；控制面后台循环每 10 分钟清理过期上传与无引用快照；被构建引用的快照仅删文件保留墓碑行；在途构建的文件绝不删除。
- 运行日志：每应用 ≤4MiB 且 ≤2000 块；指标采样保留 24h。写入时收敛，无独立清理服务。
- 构建日志：单构建 ≤20MiB（协议/journal 分块 16KiB）。

## 6. 升级与备份

- 升级顺序：备份数据库与 source-root 卷与三把密钥 → 替换二进制 → `platform migrate` → 重启控制面 → 逐台滚动重启 worker（journal 保证事件不丢，重投幂等）。
- 迁移只前滚；`001_init.sql` 为历史基线不可改写。
- 备份必须成套：PostgreSQL + source-root（归档文件）+ master-key。缺任何一个，源码授权/仓库凭据恢复即不完整。
- 会话存于 Redis，可整库丢失重建（用户重新登录）；OAuth 授权族随 oauth-key 丢失失效。

## 7. 验证状态与限制（第一期如实边界）

### 7.1 实机部署形态补充（2026-11-13/14 tencent-sh 验收实例）

- 公网入口由可信反向代理终止 HTTPS 时，控制面必须加 `--behind-tls-proxy`（豁免 gorilla/csrf 的 TLS 检查；Cookie Secure、Host 与 Origin 校验不变）。不加该标志则所有 Cookie 会话写请求 403。
- 输出仓库凭据必须先经全局管理员 VerifyRegistry 验证（真实网络/认证探测，可选读取引用），成功才标记 verified；凭据更新会重置标记，需重新验证。构建与部署均以该标记为准。
- 构建执行形态：k8s pod 内嵌套 rootless 不可行（containerd 锁定挂载+默认 seccomp），采用节点级 rootless BuildKit 守护（worker 资格 `host_build: true` + `allow_privilege_escalation: true`，宿主专用用户 systemd user 服务；需要 uidmap 包与 sysctl `kernel.apparmor_restrict_unprivileged_userns=0`）。worker 启动参数加 `--buildkit-addr`（守护 socket）与 `--buildctl-path`。守护的 buildkitd.toml 需为 docker.io 配置镜像加速（不经 containerd 加速）。
- 应用镜像必须是 non-root 兼容（构建 pod 固定 uid 1000）：官方 nginx 需换 nginx-unprivileged 变体。
- 带端口的私有仓库：凭据 auths 键为完整 host:port（缺陷⑤修复后行为）。
- k3s 1.34 已知坑：config.yaml 的 tls-san 占位符会在证书重签时导致启动循环失败；registries.yaml 生成的 hosts.toml 丢弃 auth（需手工恢复）；certs.d 目录名必须为字面 host（含冒号）。

- 已在真实环境验证：PostgreSQL/Redis 集成测试全绿；真实控制面浏览器 e2e（登录/OAuth 全协议/上传/邀请权限/管理页）；worker mTLS 登记真实链路；`make smoke`。
- **未做实机验收**（需要专用 Linux/AMD64 服务器）：k3s + BuildKit 真实构建推拉、真实域名 HTTPS 访问、Kubelet 指标精度、断网重传、PVC 保留与 NetworkPolicy 的 CNI 执行。任何"节点就绪/资格通过"在实机验收前不代表真实集群能力。
- 第一期不含：多副本、跨节点自动迁移、Git 自动部署、多容器编排、托管数据库、复杂告警/APM/Trace、计费、跨实例自动迁移。
- rootless BuildKit 不构成对任意陌生租户代码的强隔离承诺；节点权限最小 RBAC 与 NetworkPolicy 是否生效取决于安装侧 CNI，需专项验证。
- 已知权衡：stdio MCP 长会话中 PAT 过期需重启会话（启动时刷新）；e2e 连续全量运行需管理登录频控窗口。
