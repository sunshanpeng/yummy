# Yummy 第一期运维手册

面向自部署与独立托管实例的安装、配置、升级与限制说明。产品只含控制面（`yummy-server serve`）与 worker（`yummy-worker run`）两类常驻进程；CLI 为按需启动的客户端。PostgreSQL、Redis、Kubernetes（k3s）、镜像仓库为既有基础设施，本产品不额外引入服务。使用路径见 [产品文档](product.md)，实现及验证边界见 [技术文档](technical.md)。

## 1. 构建与依赖

- 工具链：Go（go.mod 声明版本，工具链 1.27.3 验证）、Node（^22/^24/≥26，pnpm 10）、make、openssl（证书）、docker（本地基础设施，可选）。
- `make build-server` 生成内嵌 Web 控制台的 `bin/yummy-server`（先执行 `make install`）。`make build-cli` 生成 `bin/yummy`，`make build-worker` 生成 `bin/yummy-worker`，这两个目标使用 Go 工具链。
- 控制面主机安装 `yummy-server`，执行宿主机安装 `yummy-worker`，用户安装 `yummy`。执行宿主机为 Linux/AMD64 时，使用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build-worker` 构建。构建 Pod 的一次性 runner 由 `cmd/build-runner` 打成镜像。
- 数据库迁移：goose 版本化迁移，进程内带锁执行；`yummy-server migrate --database-url …`。禁止 AutoMigrate。

## 2. 控制面安装与启动

配置文件（可选）：`--config /etc/yummy/config.yaml` 承载全部启动参数（数据库、Redis、监听、密钥、
OAuth、worker TLS、可观测性），键名与命令行 flag 一致，完整示例见仓库 `config.example.yaml`。
读取优先级：显式命令行参数 > 配置文件 > 环境变量 > 内置默认值；未指定 `--config` 时依次查找
`./config.yaml` 与 `/etc/yummy/config.yaml`。含密钥的文件权限设为 0600；改配置文件需重启进程生效。

1. 准备 PostgreSQL 与 Redis（Redis 仅承担会话与在线 TTL）。
2. `yummy-server migrate --database-url "postgres://user:pass@host:5432/db?sslmode=disable"`。
3. 初始化首位全局管理员（无匿名初始化接口）：
   `yummy-server admin init --database-url … --username <管理员> --password-stdin`
4. 生成三把独立密钥（Base64 32 字节）：`openssl rand -base64 32`，分别作为 session-key / csrf-key / master-key；如需 OAuth 与远程 MCP 再加 oauth-key。
5. 创建源码持久化目录（`--source-root`，必须预先创建，产品校验可写与 fsync）。
6. 启动：`yummy-server serve`。关键参数（均有同名环境变量，密钥推荐走环境变量）：

| 参数 | 说明 |
|---|---|
| `--database-url` | PostgreSQL 连接地址 |
| `--redis-addr` | Redis 地址或 redis(s) URL |
| `--public-addr` | HTTP 监听地址（默认 127.0.0.1:8080） |
| `--admin-addr` | 管理监听：/healthz 与 /metrics（默认 127.0.0.1:8082，只绑回环，公网不可达，不进反向代理） |
| `--public-base-url` | 明确的公开站点根 URL，不从请求头推断 |
| `--session-key` / `--csrf-key` / `--master-key` | 三把独立密钥；master-key 丢失则已加密的源码授权令牌与仓库凭据不可解密，必须随数据库一同备份 |
| `--oauth-key` | 留空关闭 OAuth 与远程 MCP |
| `--source-root` | 源码归档持久目录 |
| `--worker-ca-cert` / `--worker-ca-key` | worker 登记签发 CA；留空禁用登记与 worker 网关 |
| `--worker-cert` / `--worker-key` / `--worker-addr` | worker mTLS 监听的服务端证书与地址（默认 127.0.0.1:8443） |
| `--allow-local-http` | 仅开发：允许回环 HTTP；生产必须 HTTPS |

产品冒烟：`make smoke`（临时库自动清理，验证迁移→管理员→启动→登录→建空间→OAuth 元数据）。

### 身份接入

- 网页默认使用本地账号密码。启用团队登录时，配置 `oauth-key`、`oidc-issuer`、`oidc-client-id` 和 `oidc-client-secret`，并在身份提供方注册准确的回调地址 `https://<控制台域名>/auth/oidc/callback`。全局管理员在「全局管理 → 用户」为已有用户绑定身份提供方返回的 `sub`。绑定以 issuer 和 `sub` 为键，邮箱变化不影响绑定；账号创建与空间成员资格仍由 Yummy 管理。
- CLI 可在个人「访问令牌」页创建限定应用、权限和有效期的 PAT，随后用 `yummy auth token --server <控制台地址> --token-stdin` 保存。启用 `oauth-key` 后也可执行 `yummy auth login --server <控制台地址>`，通过浏览器确认授权。个人「Agent 接入」可查询和撤销已有连接。
- 在本地项目目录执行 `yummy use <空间名>/<应用名>`。确认运行配置后，`yummy deploy --dry-run` 预览文件与目标，`yummy deploy` 上传、构建、部署并等待结果。`--build-only` 停在构建阶段，`--no-wait` 返回构建 ID。两者可组合，异步且只构建。未加 `--build-only` 的异步发布由控制面在构建成功后受理，配置或运行版本在此期间发生变化时会拒绝覆盖。用 `--timeout` 调整本地等待时限。
- 已关联的目录可用 `yummy status` 查看运行状态、`yummy logs --follow` 追踪运行日志、`yummy logs --release <发布 ID>` 筛选来源发布、`yummy logs --build <构建 ID>` 查询构建日志，`yummy metrics` 查询 CPU 与内存采样。日志读取使用 `logs:read`，状态与指标使用 `runtime:read`。远程 MCP 提供 `runtime_status`、`runtime_logs`（可选 `release_id`）和 `runtime_metrics`。
- 远程 MCP 地址为 `https://<控制台域名>/mcp/apps/<应用 ID>`，可从个人「Agent 接入」复制。授权绑定单个应用和已授予的权限，成员权限变更后以当前权限为准。CLI 负责本地源码上传，MCP 可使用已有快照。
- OAuth 端点包括 `/.well-known/oauth-authorization-server`、`/oauth/authorize`、`/oauth/token` 和 `/oauth/revoke`。应用 MCP 发布 `/.well-known/oauth-protected-resource/mcp/apps/<应用 ID>`，支持受管客户端与 CIMD。依赖匿名动态注册的客户端需要预先确认兼容性。真实外部身份提供方与目标 MCP 客户端的兼容性仍需逐一验收。

## 3. worker 登记与运行

1. 控制面启用 worker CA（见上）。
2. 全局管理员创建执行节点并签发一次性注册码（全局管理或 `InventoryService.CreateNode` + `EnrollmentService.CreateNodeRegistration`）。
3. worker 机上：`yummy-worker register --server https://控制面:8443 --token <一次性码> --state-dir /var/lib/yummy-worker --server-ca-file <控制面CA>`。状态目录 0700（私钥/证书/CA/身份），注册码一次性、不可重放。
4. 安装资格：全局管理员先在控制面登记节点验收记录（`RegisterNodeQualification`，含 cluster_node_uid/OS/arch/能力位与验收证据）；worker 以 `--qualification-file` 提供同一记录，控制面逐项核对一致才置能力就绪。任何不一致或缺失即失败关闭——不自报即信。
5. `yummy-worker run` 需要显式传入 `--state-dir`、`--kube-apiserver`、`--kube-ca-file`、`--kube-token-file`、`--node-name`、`--build-namespace` 和 `--runtime-namespace`。可选 `--storage-class`、`--ingress-class`、`--runner-image`、`--buildkit-image`，镜像必须固定 digest。缺少 `--qualification-file` 时全部能力关闭。`--journal-dir` 用于持久化事件，默认是状态目录下的 `journal`。
6. 证书轮换：7 天轮换窗（`/worker/rotate` 自动）；轮换与撤销不影响在途构建下载（节点粒度信任），节点全部证书撤销才阻断。

「全局管理 → 执行节点」的集群机器清单读取默认运行节点 worker 的最新心跳。worker 服务账号需要 Kubernetes `nodes:list`，示例权限见 [RBAC 配置](../deploy/kubernetes/rbac.example.yaml)。单次最多 256 台，超过上限时标明截断。worker 离线、上报过期或权限不足时，页面显示原因；调度封锁一列反映节点的 `spec.unschedulable`。

使用 VS Code 调试时，`Go: Control` 读取常规启动配置，`Go: Worker` 按 [启动配置](../.vscode/launch.json) 将本机环境中的执行参数传入 `yummy-worker run`。

## 4. 反向代理与 HTTPS（生产必须）

- 公网入口必须由可信反向代理以 HTTPS 终止，并把全部路径（包括 `/auth/`、`/api/`、`/transfer/`、`/oauth/`、`/mcp/`、`/.well-known/`）转发到 `--public-addr` 且保留原始 Host 头（传输与会话校验 Host 必须等于 `--public-base-url` host，防 DNS 重绑定）；`/transfer/` 的大文件上传需放宽反代的 body 上限（源码上限见运行配置）。管理监听 `--admin-addr`（/healthz、/metrics）不进反向代理，Prometheus 抓取走管理网络或带访问控制的内部通道。
- 应用自定义域名 HTTPS：管理员预置 TLS Secret 并通过 `SetDomainTLSSecret` 绑定到域名声明；发布领取时全部域名绑定同一非空 Secret 才下发 HTTPS，否则失败关闭。Traefik/ACME 编排属安装侧职责。
- 本地开发可用 `--allow-local-http`。

## 5. 运行配置与数据保留

- 登录频控：20 次/15 分钟/账号与来源 IP（Redis `yummy:login:*`）。自动化测试连续全量运行前需清理该前缀键或等待窗口。
- 源码保留：快照与上传各自有到期时间；控制面后台循环每 10 分钟清理过期上传与无引用快照；被构建引用的快照仅删文件保留墓碑行；在途构建的文件绝不删除。
- 运行日志每应用最多 4 MiB、2,000 块，指标采样保留 24 小时，均在写入时收敛。升级前缺少来源发布标识的日志仍可按应用读取；按发布筛选需要升级后的控制面与 worker。
- 构建日志：单构建 ≤20MiB（协议/journal 分块 16KiB）。
- 空间管理员可在「应用 → 已归档应用」发起删除。平台等待正在执行的构建结束，再由 worker 清理集群资源。若仍有持久卷，需输入应用名单独确认。删除 PVC 前按存储回收策略核对备份和底层数据影响。清理需要更新后的 worker 和对应 RBAC，worker 离线或版本未更新时任务等待。完成后释放名称，原应用 ID 与审计、历史数据的关联继续保留。

## 6. 升级与备份

- 升级顺序：备份数据库与 source-root 卷与三把密钥 → 按同一 Git 版本替换各端二进制与启动命令 → `yummy-server migrate` → 重启控制面 → 逐台滚动重启 worker（journal 保证事件不丢，重投幂等）。
- worker 升级时沿用原 `--state-dir` 和自定义 `--journal-dir`，可直接复用登记身份并继续发送积压事件。
- 迁移只前滚；`001_init.sql` 为历史基线不可改写。
- 备份必须成套：PostgreSQL + source-root（归档文件）+ master-key。缺任何一个，源码授权/仓库凭据恢复即不完整。
- 会话存于 Redis，可整库丢失重建（用户重新登录）；OAuth 授权族随 oauth-key 丢失失效。

备份恢复应覆盖三个位置：

| 位置 | 数据与恢复顺序 |
|---|---|
| 控制面 | 暂停写入后成套备份 PostgreSQL、`source-root` 与启动配置。先恢复数据库和源码，再恢复密钥并启动同版本控制面。保留 master-key 以解密历史配置、构建密钥及仓库凭据。构建日志和运行日志随数据库备份。 |
| worker | 保留 CA 私钥和证书、服务端 TLS 证书，以及 worker 的状态目录和 journal。恢复到同一受管集群时核对节点身份和证书状态。 |
| 应用运行集群 | 单独备份 k3s 状态、入口证书、镜像仓库的必要历史镜像和应用卷数据。需要应用一致性的卷应先暂停写入，按对应 StorageClass 或存储提供方流程恢复。 |

恢复演练使用隔离实例和测试域名。检查工作空间与权限、源码下载、配置解密、日志、worker 重连，以及一个带卷应用的历史数据。保留演练结果和恢复耗时。

2026-11-25 在本机两个独立 PostgreSQL 数据库、两份源码目录和分别使用 Redis 逻辑库的控制面上完成恢复。用 `pg_dump -Fc`、`pg_restore --exit-on-error` 和源码目录归档恢复后，实测重新登录、查找原应用及历史审计、下载并校验源码 SHA-256、解密并复制历史敏感配置、拒绝错误空间下的应用访问。使用备份的 worker CA、服务端 TLS 和登记状态，通过原证书连接恢复实例并收到 HTTP/2 mTLS 网关的欢迎消息。本机样例的备份至恢复验收用时 2 秒。k3s 工作负载、运行日志与应用卷的数据恢复仍待集群演练。

## 7. 验证状态与限制

### 7.1 实机部署形态（2026-11-13 至 2026-11-18，tencent-sh）

- 公网入口由可信反向代理终止 HTTPS 时，控制面必须加 `--behind-tls-proxy`（豁免 gorilla/csrf 的 TLS 检查；Cookie Secure、Host 与 Origin 校验不变）。不加该标志则所有 Cookie 会话写请求 403。
- 输出仓库凭据必须先经全局管理员 VerifyRegistry 验证（真实网络/认证探测，可选读取引用），成功才标记 verified；凭据更新会重置标记，需重新验证。构建与部署均以该标记为准。
- 构建执行形态：k8s pod 内嵌套 rootless 不可行（containerd 锁定挂载+默认 seccomp），采用节点级 rootless BuildKit 守护（worker 资格 `host_build: true` + `allow_privilege_escalation: true`，宿主专用用户 systemd user 服务；需要 uidmap 包与 sysctl `kernel.apparmor_restrict_unprivileged_userns=0`）。worker 启动参数加 `--buildkit-addr`（守护 socket）与 `--buildctl-path`。守护的 buildkitd.toml 需为 docker.io 配置镜像加速（不经 containerd 加速）。
- 应用镜像必须是 non-root 兼容（构建 pod 固定 uid 1000）：官方 nginx 需换 nginx-unprivileged 变体。
- 带端口的私有仓库：凭据 auths 键为完整 host:port（缺陷⑤修复后行为）。
- k3s 1.34 已知坑：config.yaml 的 tls-san 占位符会在证书重签时导致启动循环失败；registries.yaml 生成的 hosts.toml 丢弃 auth（需手工恢复）；certs.d 目录名必须为字面 host（含冒号）。

- 已在真实环境验证：PostgreSQL/Redis 集成测试全绿；真实控制面浏览器 e2e（登录/OAuth 全协议/上传/邀请权限/管理页）；worker mTLS 登记真实链路；`make smoke`。
- 历史 Linux/AMD64 实机验收覆盖 Dockerfile 构建推拉、公网 HTTPS、运行日志、Kubelet CPU/内存、worker 断线重传、Pod 重建后 PVC 数据保留，以及跨命名空间入站隔离。示例应用见 [部署示例](../examples/README.md)。
- 仍需验收 Railpack 纯源码构建到 HTTPS 的完整流程，并补做 k3s 工作负载、运行日志和应用卷的恢复演练，以及真实集群确认删卷和名称释放的验证。规划器固定为官方 Railpack v0.43.0，前端镜像用 `--railpack-frontend-image` 指定同版本 digest，宿主构建另需 `--railpack-path` 指向该版本二进制。Pod 模式目前通过日志查看规划阶段，任务失败对外仍使用通用构建失败码。
- 发版时需用目标远程 MCP 客户端完成登录和实际应用操作，并补验控制台读屏和 WCAG AA。已有协议及浏览器自动化证据见 [技术验证清单](technical.md#8-测试与实机验收)。
- 第一期不含：多副本、跨节点自动迁移、Git 自动部署、多容器编排、托管数据库、复杂告警/APM/Trace、计费、跨实例自动迁移。
- rootless BuildKit 不构成对任意陌生租户代码的强隔离承诺；节点权限最小 RBAC 与 NetworkPolicy 是否生效取决于安装侧 CNI，需专项验证。
- 已知权衡：e2e 连续全量运行需管理登录频控窗口。
