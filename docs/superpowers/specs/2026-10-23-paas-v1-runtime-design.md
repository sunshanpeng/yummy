# Yummy 第一期：从本地源码到可运行应用

2026-10-24 交付模型修订：基础设施由实例管理员统一管理，工作空间不能自定义；以 [交付方式与实例边界](2026-10-24-delivery-model-and-instance-boundaries-design.md) 为准。

状态：待评审设计，2026-10-23。本文将第一期对外交付从“产出镜像”修订为“应用部署与日常运行的完整闭环”。配套：[产品方案](2026-10-22-agent-first-build-product-design.md)、[构建基础技术方案](2026-10-22-agent-first-build-technical-design.md)、[前端方案](2026-10-22-agent-first-build-frontend-design.md)、[OAuth 方案](2026-10-23-oauth-mcp-auth-design.md)。当前仍为方案与静态原型，未实现真实部署或监控后端。

2026-10-24 的创建与资源边界修订以 [创建与配置边界复核](2026-10-24-application-configuration-boundaries-design.md) 为准。

## 1. 首发成功标准

非作者用户能在自有服务器接入节点和仓库，在应用中配置运行参数，通过网页或 Agent 上传本地源码与 Dockerfile，在服务器构建、部署并获得可访问的 HTTPS 地址；之后可以修改变量、查看运行日志和 CPU/内存趋势、更新版本、回滚和停止/启动应用。

首期覆盖单容器 HTTP 服务、静态站点容器、后台任务进程；后台进程不创建域名。首期每应用一个运行实例、一个运行目标，支持基础持久卷与配置文件挂载。多副本伸缩、多节点迁移、多容器 Compose、托管数据库、完整 APM/Trace、任意 PromQL 与复杂告警后置。这里的“首期”是完整对外版本，下面的研发里程碑是内部顺序，构建功能独立完成不等于首期已可发布。

业务层级仍只有工作空间 → 应用。每个应用首期只有一套运行配置，不增加项目或环境导航；未来多环境作为应用内维度扩展。常驻产品进程仍只有控制面和 worker，OAuth、调度、部署编排、采集与清理都在现有进程内组织。

## 2. 竞品依据与明确取舍

[Dokploy 应用文档](https://docs.dokploy.com/docs/core/applications)将部署、变量、监控、日志、域名与资源/卷配置放在应用内；[Coolify 变量文档](https://coolify.io/docs/applications/configuration/environment-variables)区分构建与运行阶段。Yummy 采用这些基础使用能力，不把首发做成只产镜像的构建工具。

[Coolify 回滚文档](https://coolify.io/docs/applications/deployments/rollbacks)说明镜像回滚不恢复数据库和文件，且其回滚使用当前运行配置。Yummy 本版选择不可变 release，同时绑定镜像 digest 与运行配置版本；用户回滚前能看到两个版本的差异，仍不回退持久数据和外部系统。

## 3. 用户路径与页面

```text
管理员：接入执行节点 → 仓库 → 入口域名/HTTPS → 创建应用与成员授权
开发者：配置端口/健康检查与变量 → 上传源码 → 构建并部署
运行确认：新实例就绪 → 业务入口验证 → 展示访问地址和运行版本
日常操作：日志 / 监控 / 变量 / 配置 → 更新发布 / 回滚 / 重启 / 停止启动
```

工作空间首页只显示应用，卡片的主要状态是“运行中 / 发布中 / 不健康 / 已停止 / 未部署 / 状态未知”，另标最近构建状态。新构建失败时，旧版本可能继续运行，不能把“最近构建失败”当作“应用离线”。

应用详情是独立页面，页签如下：

| 页签 | 首期内容 |
|---|---|
| 概览 | 当前运行版本、健康/访问状态、域名、执行节点、资源摘要、待发布配置和近期事件 |
| 部署 | 发布历史、镜像与配置版本、阶段/失败证据、重新部署、选定版本回滚 |
| 构建 | 源码上传、构建并部署/仅构建、构建日志与镜像；镜像产物作为本页子视图 |
| 日志 | 运行日志，按实例、部署和时间范围查询/跟随；与单次构建日志明确分开 |
| 监控 | CPU、内存、就绪状态、重启及 HTTP 探测结果，最近 1 小时/24 小时 |
| 变量 | 运行时变量、敏感值替换、批量导入预览；构建参数另设明显分区 |
| 配置 | 运行方式、端口、域名/HTTPS、健康检查、CPU/内存、配置文件、持久卷、构建设置 |

节点、输出仓库与平台默认域名放实例级平台管理；空间管理只含成员身份、空间角色、应用授权概览与空间资料。节点页面改称“执行节点”，分别显示构建/运行/监控能力；应用成员只读查看平台资源摘要，不选择节点/输出仓库；构建密钥默认属于应用，引用需要校验本应用归属。

## 4. 发布、配置和运行状态

### 4.1 数据对象

| 对象 | 关键事实 |
|---|---|
| `Application` | workspace_id、应用类型、构建默认配置、当前配置草稿/已保存版本、desired_release_id、serving_release_id、运行意图与观察状态 |
| `Build / Artifact` | 现有不可变源码/构建；验证过的 repository + digest + 平台，不使用可变 latest 发布 |
| `ApplicationPlacement` | 系统分配的运行节点/集群身份；首次部署时固定，改变实例默认策略不迁移既有实例 |
| `RuntimeConfigRevision` | 应用类型、端口、启动参数、探针、资源限制、变量/文件的版本引用、域名和卷绑定 |
| `Release` | application_id、artifact_id、runtime_config_revision_id、placement/首发目标快照与策略版本、前一成功版本、发起者、来源 build/rollback、创建时间；创建后不可改 |
| `DeploymentOperation` | release_id、operation_id、单调 generation、目标 worker、阶段、结果、重试/恢复事实 |
| `RuntimeObservation` | 节点、Deployment/Pod UID、观察 generation、Ready/重启/镜像身份、入口状态、采集时间 |

PostgreSQL 保存这些对象，GORM Gen 与 Goose 继续适用。变量秘密、文件秘密单独加密存储并按版本引用；公共 DTO 只返回结构与已配置状态，不能把明文藏在未渲染 JSON 中。运行配置与构建配置分开，修改运行变量不需要重建镜像。

### 4.2 保存与生效

- 创建仅填写名称，随后进入配置页。HTTP 类型默认端口与探针路径仅是建议，首发前必须确认；没有代码或资源策略时仍允许先创建未部署应用。只构建不要求运行配置已确认，自动部署需要确认。
- 变量/运行设置先编辑草稿；“保存配置”产生新 revision，显示“未部署的更改”，当前运行版本不改变。
- “部署更改”复用当前已验证镜像和新配置创建 release，无需再构建。首次部署没有镜像时引导上传构建。
- 选择“构建并部署”时，在提交时绑定配置 revision 和自动发布意图；构建完成前用户又保存了配置，也不能静默把新草稿加入旧请求。
- 仅构建仍是可选动作；构建失败或产物验证失败不触发部署。构建成功不等同部署成功。
- 启动参数以数组表达，默认沿用镜像 ENTRYPOINT/CMD，不在宿主拼接 Shell。HTTP 监听端口为明确配置；Dockerfile EXPOSE 只能作建议，不能作为已验证监听事实。
- 运行变量支持普通值和敏感值、校验名称/重复项、导入 `.env` 前差异预览。敏感旧值只替换不回显；批量导入不以掩码覆盖真实秘密。构建参数与 BuildKit secret 继续沿用独立契约。

### 4.3 状态与失败语义

部署阶段：排队 → 应用配置 → 拉取镜像/创建实例 → 等待新实例就绪 → 验证访问 → 成功；失败记录具体阶段和 Kubernetes 事件。后台进程跳过业务入口验证。入口缺少 DNS、证书申请中、进程未就绪分别展示，不用一个“失败”覆盖全部原因。

应用运行状态与最近发布结果分开。例如“v12 正在运行 / 发布 v13 失败”，不能把发布失败的 v13 标成当前成功版本。worker 失联时展示最后观察时间与状态未知，不继续显示实时健康。首期不提供中途强制取消发布；可停止应用或恢复上一成功 release。停止会提升期望 generation 并取代活动发布，worker 串行收敛到停止，旧操作不得再次启动实例；不能把停止请求堵在一个失控发布之后。

## 5. k3s 部署与入口

### 5.1 复用现有 worker

每个执行节点仍是一个 worker 对应单节点 k3s。节点具有 build/runtime/metrics 能力标记，构建和运行可在同一节点，运行负载需预留资源，构建并发不能耗尽应用的保底资源。运行能力未就绪不能通过“仅构建就绪”校验。

控制面内的持久操作扫描器发送类型化 Deploy/Start/Stop/Restart/Reconcile 消息；worker 内的运行管理器通过 client-go 创建/更新 Deployment、Service、Ingress、ConfigMap、Secret 和 PVC。实例控制、调度、重启交给 Kubernetes 原生控制器；不新增自有 operator 进程、不用 SSH 执行 kubectl、不引入 Argo CD。

每个应用只有一个活动运行变更操作，数据库锁定并递增 generation；worker 持久保存已处理 generation，并在每次变更前验证当前期望值，旧消息不能覆盖新 release。资源命名确定，写入 workspace/application/release/generation 标签，重复发送只核对与恢复同一操作。重启通过 operation_id 对应的 Pod template 标记实现幂等，不因消息重投反复重启。

首期由实例管理员为工作空间分配独占执行范围；工作空间不能管理节点。业务命名空间与构建命名空间分离。worker 权限仅覆盖本空间受管命名空间的必要资源；Kubelet 监控另授予本节点 `nodes/metrics get`，不为采集授予 `nodes/proxy`。应用容器不挂宿主 socket、worker 私钥或 Kubernetes token，不开放 privileged/hostNetwork。

### 5.2 成功、更新与恢复

使用[原生 Deployment](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)及[启动/就绪/存活探针](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)。默认一实例；无持久卷时采用允许 1 个 surge、0 unavailable 的滚动策略，容量不足时明确失败，不承诺任意条件下零停机。有持久写卷的应用首期采用 Recreate，页面提示更新会短暂中断，避免两个写进程并行。

成功条件至少包括：目标 generation 被观察、新实例运行正确 digest、目标副本就绪、旧副本已退出、Service 端点匹配；HTTP 服务还要完成入口/TLS 与配置健康路径的访问检查。apply 返回成功、Pod Running、一个 HTTP 200 均不能单独证明完整发布成功。

新版本失败时将操作记为失败；控制面可以按默认策略提交一次恢复操作到前一成功 release，并明确显示恢复中/恢复成功/恢复失败。Kubernetes Deployment 不会自动替平台实现完整回滚策略，不能只依赖 progressDeadlineSeconds。首发失败没有可恢复版本则保持不健康并给出证据。

发布与回滚前核对目标 manifest 和所需配置/秘密版本仍可用；外部仓库清理导致镜像丢失时明确拒绝，当前服务不变。平台保留引用不能阻止外部仓库独立删除镜像。

worker/control 重启后从 PostgreSQL 期望态与 k3s 资源标签恢复，查证正在运行的实际版本；不盲目再创建第二个实例。账号停用阻断新操作，但不会自动关闭已上线业务。显式停止把副本调整为 0，保留配置、历史和持久数据；启动复用选定 release，仍需经过就绪与访问确认。

### 5.3 域名与 HTTPS

复用 k3s 的 [Traefik](https://docs.k3s.io/networking/networking-services)入口。管理员配置业务根域名及 DNS 指向、80/443 和 ACME 邮箱；平台以不可变应用 ID 为 HTTP 应用分配实例内唯一的默认子域名（只读），应用可以另外填写可选自定义域名并进行 DNS 检查。域名按实例唯一占用并绑定应用授权，不能通过修改域名接管其他应用入口。

首期采用单 Traefik 实例的 [ACME HTTP-01](https://doc.traefik.io/traefik/reference/install-configuration/tls/certificate-resolvers/acme/)，显式持久化证书存储并验证续期/重启恢复。每个实际域名单独签发证书；泛域名 DNS 不等于已有泛域名证书。多入口 HA 与 DNS-01 提供方集成后置，不默认新增 cert-manager 服务。内网无法完成公开 ACME 时支持管理员预置证书，或明确使用 HTTP，不显示虚假的 HTTPS 通过状态。

### 5.4 配置文件与持久存储

配置文件按路径、内容/敏感标记、版本保存，以只读 ConfigMap/Secret 文件挂载进入容器；不挂宿主文件，不通过 build ARG 写入秘密。挂载路径和冲突在发布前校验，按变更产生新配置版本并重建实例，不假定进程会自动热加载。

基础持久卷使用固定 StorageClass/PVC 与应用绑定，初始容量建议 1 GiB。k3s local-path 只承诺当前节点的持久性，不能等同共享存储、跨节点故障恢复或备份。停止、重部署、回滚不删除 PVC；删除应用默认保留数据，删除卷是管理员单独确认的操作。后续数据库托管再提供专门备份恢复流程；首期应用可用运行变量连接外部数据库。

## 6. 基础监控与运行日志

### 6.1 监控来源与限界

首期必须有应用资源历史。采用 worker 内嵌采集器，读取本节点 Kubelet `/metrics/resource`，使用成熟 Prometheus 文本解析库，不新增采集进程。读取 `container_cpu_usage_seconds_total`、`container_memory_working_set_bytes` 与容器启动时间；按受管 namespace/Pod/容器解析回实际 Pod UID、application、release，平台与其他应用数据不混入。

[Kubernetes 指标参考](https://kubernetes.io/docs/reference/instrumentation/metrics/)列出了这些资源指标；[Kubelet 授权](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-authn-authz/)支持节点 metrics 子资源。TLS、Kubelet Webhook 授权与本节点 resourceNames 必须在 M0 验证，不能关闭证书验证或回退到集群管理员凭证。

不把 Metrics Server 轮询后包装成监控历史：[其官方说明](https://github.com/kubernetes-sigs/metrics-server)明确其用途是自动伸缩，监控应直接采集 Kubelet 资源端点。k3s 自带该组件不代表已经具备历史监控系统。

建议每 30 秒采集。CPU 用相同容器启动身份下的 counter 差值/实际时间差计算核数，界面可显示 mCPU；counter 重置/首样本不足时显示待采集。内存使用 working set，按 MiB 展示；百分比明确以资源限制为分母，不把整机核数、应用 limit 和 request 混为一谈。

PostgreSQL `runtime_metric_samples` 保存有界固定字段快照，按 application/time 索引、去重并保留 24 小时；1 小时/24 小时图表按明确的每分钟均值返回 count 与最新采集时间。这里只是首期资源趋势，不实现通用 TSDB、标签查询引擎或 PromQL；长期历史通过查询/导出适配接入成熟指标后端。

就绪数、重启计数、OOM/拉取失败等来自 Pod 状态与事件；HTTP 可达性和探测延迟来自控制面内对已登记入口的探测，目标由已校验应用路由确定，不接受任意探测 URL。探测延迟不称为业务请求 P99/QPS。首期不承诺应用网络/磁盘吞吐、请求错误率或 Trace，不能用 CPU/内存数据伪造这些指标。

采集失败、节点失联、应用停止时分别显示缺测/陈旧/已停止，曲线断开，不补 0。运行指标、探测、日志都带采集时间与 release 关联。基础异常在概览/监控/事件里可见，复杂规则告警、外部通知编排后置。

### 6.2 日志

worker 使用 Kubernetes 日志 API 跟随当前及必要的前一次容器日志，复用已有的有界日志块、持久暂存与 ack 通道，按 Pod UID + container + restart + seq 去重；构建和运行日志分别存储、分别授权。

运行日志支持按当前/历史 release、实例、时间范围读取，分页游标与应用绑定；界面只对已加载结果做文本查找，并明确范围。建议默认保留 24 小时且每应用最多 50 MiB，按先达到的限制清理；截断、Pod 轮转和未恢复缺失明确标记。stdout/stderr 是应用日志入口，平台不承诺替应用收集任意磁盘文件。

变量与凭证不写平台日志、审计或发布差异；应用主动把秘密打印到 stdout 时无法保证自动消除，因此敏感日志仍按应用权限隔离。没有运行实例或历史日志过期时展示真实空状态。

## 7. 权限、Agent 与接口

| 操作 | 空间 admin | 应用 maintainer | 应用 developer | 应用 viewer |
|---|---|---|---|---|
| 查看部署、运行状态、日志和监控 | 是 | 是 | 是 | 是 |
| 构建并部署、部署已有产物、回滚、重启 | 是 | 是 | 是 | 否 |
| 修改变量、端口/域名、资源/卷与运行配置 | 是 | 是 | 否 | 否 |
| 停止/启动应用 | 是 | 是 | 否 | 否 |
| 管理执行节点、入口基础设施和成员授权 | 是 | 否 | 否 | 否 |
| 读取已存敏感值明文 | 否 | 否 | 否 | 否 |

Casbin 增加 deployment、runtime、runtime-config、runtime-logs、runtime-metrics 动作，仍按当前用户权限 ∩ 应用范围 ∩ token scope。OAuth 增加 `deployment:read/write`、`runtime:read/write`、`config:read/write`，`logs:read` 覆盖获授权应用的日志；默认只读，发布/运行变更和配置修改分别授权。敏感配置 read 只返回键与版本，不回明文。

Connect/CLI/MCP 共用应用服务：GetApplicationStatus、List/GetDeployment、CreateDeployment、RollbackDeployment、Restart/Start/StopApplication、Get/SaveRuntimeConfig、ReadRuntimeLogs、QueryRuntimeMetrics。操作返回 operation/release ID、已绑定版本、阶段、可重试性和详情 URL，不把等待中当作成功。更新带 expected_config_revision/expected_serving_release，防止 Agent 覆盖别人刚刚发布的版本。

本地上传仍由 CLI/stdio 完成；远程 MCP 对已上传快照或已验证 artifact 操作。Skill 增加读取运行配置、预览发布差异、发布、确认可用、读运行日志/指标和回滚路径。用户要求部署时可按既有授权执行；平台不因 Agent 接入自动开放管理员或秘密读取能力。

拟议 CLI：

```bash
platform build submit . --deploy --output json
platform application status --output json
platform deployment list --output json
platform deployment get rel_example --output json
platform application logs --follow
platform application metrics --since 1h --output json
platform deployment rollback rel_previous --expected-current rel_example
```

## 8. 研发顺序与实际验收

| 内部里程碑 | 内容 | 验证 |
|---|---|---|
| M0 | 固定 k3s/BuildKit、运行权限、Traefik/TLS、Kubelet 采集与持久卷 | 三个样例的构建/拉取/就绪/HTTPS、资源指标与卷保留实际记录 |
| M1 | 身份/资源/应用、上传构建、产物、基础 CLI/页面 | 既有权限矩阵与 Apple Silicon 上传→AMD64 构建 |
| M2 | 配置/变量/文件/卷、发布、域名、健康、运行日志 | 从零部署到真实 URL；改变量不重建镜像且保存未发布不影响线上 |
| M3 | CPU/内存历史、更新失败恢复、手动回滚、停止启动、OAuth/MCP 完整操作 | 错误新版本保持/恢复旧版本；回滚匹配配置；查看者和越权令牌拒绝 |
| M4 | 断线重启恢复、配额/清理、团队试用与备份说明 | 重投不重复发布/重启；失联显示未知；PVC 与当前/回滚引用不会被清理 |

最终验收：外部用户分别部署 Go HTTP、Node HTTP、静态站点与后台进程；HTTP 可通过真实 HTTPS 访问，worker 不伪造域名；验证首发失败、发布失败但旧版本运行、证书/DNS 待处理、采集缺失、日志轮转、修改敏感变量、回滚配置、PVC 保留和控制面/worker 重启。

本轮只交付文档与可点击原型。协议库、Kubernetes 实际清单、证书签发、监控精度、运行日志持久化和故障恢复均须在实现阶段按以上标准验证，不能以示例图表或“模拟发布成功”替代。
