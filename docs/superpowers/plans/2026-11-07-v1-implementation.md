# Yummy 第一期完整功能实施与验收计划

## 目标与范围

将现有 uppercase 多拓扑 Demo 演进为真实的源码上传、构建、应用部署与运行平台。完成标准是设计要求的应用运行闭环，而不是页面可点击或镜像构建成功。直接在当前分支工作，不创建额外 worktree。

设计以 `AGENTS.md` 为最高仓库约束；2026-10-24 的交付模型、创建与配置边界覆盖旧稿中空间管理基础设施的描述；2026-10-25 的前端设计修订确定个人账户独立、构建密钥归应用、应用授权由空间管理员维护。旧运行方案权限表中允许空间管理员管理执行节点的行不适用。

包含：本地账号/邀请/权限/个人凭证，工作空间与应用，实例节点/仓库/执行策略，安全源码上传，持久化构建与日志，产物校验，运行配置/变量/文件/卷，部署/域名/HTTPS/健康，日志/CPU内存历史，发布恢复/回滚/启停，CLI/PKCE/OAuth/本地与远程 MCP，正式 React 控制台，测试和运维文档。

不包含：多副本、跨节点自动迁移、Git 自动部署、自动语言构建、多容器编排、托管数据库、复杂告警/APM/Trace、计费系统、跨实例自动迁移、面向陌生租户的任意恶意代码隔离承诺。三个交付方式共用产品内核，不建设三个代码分支或额外身份/日志/部署服务。

## 实施假设与权衡

- 产品只保留控制面和 worker 两类常驻进程。CLI/stdio MCP 按需运行；Kubernetes、Redis、PostgreSQL、仓库保留其基础设施职责。
- 首期单控制面。PostgreSQL 是权限与任务事实来源；Redis 承担服务端会话与在线 TTL，不是构建队列。
- 先保留旧 Demo 表与回归测试，产品接口独立使用 `platform.v1`；Demo 最终只在显式开发模式开放。
- 新模块采用 GORM/Gen、Goose、Gin、Cobra/Viper、Casbin、Fosite、官方 MCP SDK，版本以可下载的稳定版本和兼容测试锁定。迁移不重写已执行的 `001_init.sql`，不使用生产 AutoMigrate。
- 前端采用 React SPA、Tailwind 4、shadcn Radix、TanStack Router/Query/Table/Form、Zod、Lucide 和 Recharts，不增加 Node SSR。中性浅色、语义变量、无主题切换；异步操作不得乐观宣告发布成功或停止。
- 设计技能检索已执行；自动推荐的深色、横向落地页和营销布局不适用于本项目，不采用。保留键盘、焦点、错误关联、响应式和减弱动画规则。
- 首期认证执行平台是 Linux/AMD64。当前开发机为 ARM Mac，模拟 Kubernetes 单测不能替代真实节点资格验证；缺少真实环境时记录为未验收，节点不能冒充就绪。

## 分阶段实施

> 状态指针：各阶段标题标注本地验证状态；条目勾选以验收矩阵（2026-11-11 独立审查报告）与历轮"验收记录/接续记录"为准。实机验收条目保持未完成。

### P0：基线、契约和环境（部分——工具链锁定与测试基线已验证；实机环境获取待专用服务器）

- [x] 阅读全部设计与最新边界，梳理真实 Demo 与静态原型差异。
- [x] 确认当前分支干净、Docker 可用；找到缓存的 Go 工具链。
- [ ] 锁定可用 Go/Node/协议与依赖版本，跑通现有测试、存储集成基线。
- [ ] 记录真实 AMD64/k3s/BuildKit、仓库和 DNS/HTTPS 环境获取方式。

### P1：安全的持久化产品内核（本地真实验证通过——验收矩阵与历轮记录见下）

- [ ] Goose 版本化迁移及既有 schema 基线、迁移锁；GORM 模型与 DAO。
- [ ] 本地命令初始化首位管理员，无匿名初始化接口；Argon2id 密码、限流。
- [ ] Redis 不透明会话、Cookie/CSRF/同源校验、账号停用与密码重设即时失效。
- [ ] 固定角色 Casbin，实例/空间/应用三类权限，列表按授权过滤、对象归属校验。
- [ ] 工作空间、成员/邀请、名称-only应用、应用授权、最后管理员保护、审计。
- [ ] 个人 PAT 摘要存储、一次性明文、有效期/动作/应用范围与当前权限交集。
- [ ] 产品 Protobuf/Connect API 与统一错误码、capabilities。

### P2：实例资源、源码与构建（本地真实验证通过——验收矩阵与历轮记录见下）

- [ ] 实例节点/仓库/版本化策略，应用只读解析结果；每应用独立 repository。
- [ ] 一次性节点注册码、CSR/证书签发/轮换/撤销、能力和资格检查。
- [ ] CLI 本地目录授权与 Docker ignore 语义；安全打包、摘要、敏感文件排除。
- [ ] 上传会话、流式有界传输、安全校验与原子 READY 快照、受限下载。
- [ ] 构建幂等/请求指纹、事务冻结策略/源码/配置/密钥版本、容量锁与队列扫描。
- [ ] 同一 attempt 的 Run/Cancel/Reconcile、事件序号去重、持久日志与 ack。
- [ ] k3s Job + 固定 BuildKit runner、受限资源/RBAC/网络、安全解包、manifest/config 独立校验。
- [ ] 取消/超时/断线/Job消失/重启恢复的正确状态，未知不误报停止。

### P3：真实应用运行（本地真实验证通过——验收矩阵与历轮记录见下）

- [ ] 运行配置不可变版本；普通/敏感变量、命令、探针、资源、文件和 PVC 校验。
- [ ] 保存与部署分离；Release 绑定 digest + config revision，Placement 首次受理冻结。
- [ ] Deployment/Service/Ingress/ConfigMap/Secret/PVC，期望 generation 与操作幂等。
- [ ] 默认与自定义域名占用、DNS/入口/TLS 检查；Traefik ACME/预置证书。
- [ ] Ready/正确镜像/旧实例退出/入口健康验证，失败恢复、手动回滚和重启/启停。
- [ ] 运行日志持久块与游标、Pod/restart身份；Kubelet CPU/内存采集与24小时有界历史。
- [ ] 陈旧、缺测、停止分别表达；无实例无假数据；回滚不恢复数据库/卷。

### P4：CLI、OAuth 与 MCP（本地真实验证通过——验收矩阵与历轮记录见下）

- [ ] Cobra 业务 CLI、上下文、JSON 输出、上传/构建/部署/日志/监控，安全凭证存储。
- [ ] Fosite PostgreSQL 适配、PKCE S256、明确 issuer/resource/audience、同意/拒绝。
- [ ] 刷新轮换/族重用撤销、scope不扩张、即时撤权、个人连接管理。
- [ ] 标准 OAuth/受保护资源发现、CIMD SSRF防护、预注册客户端。
- [ ] 官方 SDK 的本地 stdio 和应用专属远程 HTTP MCP，直接复用应用服务。
- [ ] 可选外部 OIDC 登录：issuer+subject绑定，不按邮箱自动合并或授空间权限。

### P5：正式 React 控制台（本地真实验证通过——验收矩阵与历轮记录见下）

- [ ] 落地正式依赖和语义设计 token、shadcn 组件、嵌套路由与 Query缓存边界。
- [ ] 登录/邀请/账户/PAT/Agent/OAuth页面。
- [ ] 工作空间应用首页、名称-only创建、独立应用详情及构建/部署/日志/监控/变量/配置。
- [ ] 工作空间管理与实例全局管理分离；应用构建密钥与授权入口。
- [ ] 服务端分页/筛选、冲突/加载/空态/错误、无权限、键盘与响应式。
- [ ] Vitest 与真实后端 Playwright 跨页用例；不以假数据测试替代端到端验收。

### P6：安全、恢复与交付（本地项通过；实机验收待专用服务器）

- [ ] PostgreSQL并发/幂等/队列锁/状态条件/撤权集成测试。
- [ ] 源码攻击样例、OAuth负向、跨域/跨应用/跨scope、密钥不泄漏回归。
- [ ] 控制面/worker重启、日志重传、超时/取消竞态、失败恢复与PVC保留。
- [ ] Go HTTP、Node HTTP、静态站点、后台进程真实构建部署；HTTP真实HTTPS访问。
- [ ] 安装/升级/配置/证书/备份恢复/配额清理/限制说明及兼容记录。
- [ ] 独立实现审查与全量测试；逐项报告通过、未通过和需要外部环境的验收，不将目标提前标完成。

## 验收记录

| 日期 | 项目 | 结果 |
|---|---|---|
| 2026-11-07 | 原有 React 组件测试 | 1个测试通过；当前 Node 25 不在声明支持范围，将使用受支持版本复验 |
| 2026-11-07 | Go/数据库基线 | 原有 Go race、真实 PostgreSQL/Redis 集成测试全部通过；工具链自动获取 Go1.27.3 |
| 2026-11-07 | P1 内核首轮 | Goose 接管、组合约束、身份/PAT/邀请/撤权、工作空间/应用服务与 HTTP Cookie/CSRF 测试通过，继续补端到端接线 |
| 2026-11-07 | 源码归档纯库 | race/vet通过，覆盖率86.2%；fuzz 351710次无panic；Linux仅交叉编译，未作为实机验收 |
| 2026-11-07 | 工具链兼容 | Gen0.3.29间接dbresolver1.5.3不兼容GORM1.31.2，固定dbresolver1.6.2后生成与测试通过；Node24.22.2已在work隔离安装 |

## 2026-11-10 接续记录

- 当前工作保留在 `main`，仓库中存在大量此前已修改及未跟踪的文件；不要执行 reset、clean 或覆盖他人变更。
- 本轮实现：构建专用源码授权按 attempt、节点及证书绑定，令牌以摘要认证、加密保存，重复派发复用；下载走独立 `/transfer/build-grants/` 入口，取消、证书撤销或过期后不能新开下载。下载端是任务容器中的受限 Bearer，并非 mTLS 身份认证；证书绑定在控制面授权记录及有效性检查中。下载已开始后的数据流不追溯撤回；公网部署必须由可信反向代理以 HTTPS 终止并转发此入口。
- 尚需设计证书轮换时已有构建授权的安全恢复：当前授权绑死旧证书，撤销/轮换后旧令牌拒绝下载，不能默认为同 attempt 自动换令牌或重建 Job；须结合 Kubernetes Secret 幂等、Job UID、取消和重连协议处理。源码文件自动清理也尚未接入。
- 构建密钥及仓库凭据按冻结版本下发；发布仓库凭据下发前复核操作、镜像目标、节点证书及 generation。测试中的资格、产物或证书 fixture 不代表实际节点或仓库合格。
- kube 恢复不因临时凭据缺失而重新创建已确认 Job；停止不依赖历史仓库凭据，依然核验 release、镜像和所有权。网关关闭等待活动流清理。部署重投使用数据库时钟判断与设置租约。
- 真实产品 worker 入口、Kubernetes 执行器装配、控制面调度/恢复/产物验证循环、真实能力资格验证、自动发布、入口 HTTPS 探测、运行日志和监控链路、完整本地 stdio MCP、OAuth 客户端管理及端到端验收仍未完成。当前 capability verifier 缺省失败关闭，不可通过自报或 fixture 宣告真实 ready。
- 本次全量 `go test ./... -race -count=1` 在本机隔离 PostgreSQL/Redis 上通过；随后材料目标一致性检查和数据库时钟租约调整又通过 `server`、`source`、`workergateway`、`deployment` 包的 `-race` 回归。Mac 上的 fake Kubernetes 测试不替代专用 Linux/AMD64 k3s、BuildKit、registry、DNS 和 HTTPS 实机验收。
- 用户要求当前切片完成后暂停，并在新会话继续。截图确认 `zai-coding-cn / glm-5.3-flash` 已在子智能体设置勾选，但设置注明只影响新会话；旧会话 `list_subagent_models` 只返回 Codex，直接查询该提供商显示此会话不允许。新会话应先重新列出允许模型，再按用户要求选择 GLM，不擅自替换其他模型。

## 2026-11-10 深夜接续记录（GLM 子智能体协作切片）

- 模型说明：主会话切换后本会话子智能体提供商变为 `zai-coding-cn`，但初始仅允许 `glm-5.3`；显式选择 `glm-5.3-flash` 报"未记录模型选择策略时显式选择限于父路由"。用户重存设置无效（策略仅新会话生效），确认后本轮 A/C 用 `glm-5.3`；主会话再次切换后 `glm-5.3-flash` 子路由实测可用，B 的收尾由 `glm-5.3-flash` 完成。后续新会话按用户要求优先 flash。
- 本轮三个 GLM 子智能体并行（文件边界互斥）：A=deployment 自动发布/入口探测/观察收敛；B=workerexec 真实 worker 执行器 + `platform worker register/run` CLI；C=node_qualifications 迁移 016、inventory 资格登记/撤销/列表（proto 已生成）、qualification.Verifier（失败关闭，能力位取自登记记录）。B 曾中途失败，遗留半成品由主会话重新委派 flash 补全。
- 主会话整合：`server/lifecycle.go` 新增请求外循环（构建恢复 5s、产物独立验证 30s、自动发布 10s、入口 HTTPS 探测 15s、观察收敛 30s）；网关装配 `Capabilities: qualification.NewVerifier(db)` 与自动发布/探测/收敛循环；修复 inventory 资格列表分页为 `(created_at, id)` 复合 keyset（随机 id 不代表创建顺序）。
- 全量回归暴露第二处真实缺陷（本机 Docker Desktop 虚拟机时钟漂移放大暴露）：deployment/build 写入 `next_dispatch_at` 用宿主机 Go 时钟，领取侧用数据库 `NOW()` 比较，混合时间基准使到期任务在时钟偏差下不可领取（deployment 两测试确定性失败，数据库曾落后宿主约 4ms~更久）。已统一为数据库时钟：deployment 新建操作与 build 停止重投/领取租约均以 `gorm.Expr("NOW()")`/`NOW() + INTERVAL` 写入。后续新增任何与 `NOW()` 比较的时间戳必须用数据库时钟写入。
- 验证：平台全部包 `-race` 回归通过（除 workerexec 当时半成品）；修复分页后 inventory 全绿；workerexec/cli 由 B 报告全绿且主会话复跑大多通过，但 `TestDeployHTTPCreatesDeploymentServiceAndObservation`/`TestDeployRedeliveryDoesNotDuplicateTemplates` 在全包 `-race` 压力下偶发失败。经 goroutine 转储→done 错误暴露→fake 操作序列转储→归属校验错误加资源上下文逐层定位，根因是 kube.Observe 的真实中间态缺陷：Deployment 控制器先建 ReplicaSet 再建 Pod，两次 List 之间出现新 Pod 时旧代码误判 ErrOwnership 并杀死正在收敛的 Deploy。已改为跳过本轮流数（OldPodsExited=false 失败关闭，不计副本/就绪），标签与节点不匹配的外来对象仍硬拒绝；kube 归属拒绝错误补充资源上下文便于运维定位。测试 waitFor 超时时转储 goroutine、Deploy 测试以 waitObservationOrDone 直接暴露提前失败原因。60 次全包压测确认后此条移除保留结论。
- B 报告的协议缺口：wire 协议 `WorkerDeploymentCommand` 未携带 Ingress TLS Secret 引用/HTTPS 标记，worker 只创建明文路由，不伪造 HTTPS 证据；域名 TLS 由控制面预置证书 + 探测负责。真实 HTTPS 端到端仍未验收（无专用 Linux/AMD64 服务器）。
- wave 2 进展（GLM flash 子智能体协作）：D 期完成运行日志/指标上行纵切——worker.proto 新增 runtime_log/runtime_metric envelope、deployment.proto 新增 GetRuntimeLogs/GetRuntimeMetrics、迁移 017（幂等键 + 写入事务内配额收敛：日志每应用 ≤4MiB/≤2000 块、采样 24h）、workergateway Emitter/Journal/adapter 扩展、deployment 服务与 transport 查询（键集分页、application:read 鉴权）；时间戳全部数据库时钟。主会话补齐 workerexec 测试桩新接口方法。F 期完成应用详情页运行日志查看器（键集分页=加载更新、Pod/容器过滤、跨块流式 UTF-8 解码、role=log/aria）与 shadcn ChartContainer CPU/内存图表（chartConfig 语义变量 var(--chart-N)、窗口 Tabs、单点降级、未部署空态不发请求）；web 31/31 测试与构建通过、硬编码色值零命中、未新增依赖。E 期完成 workerexec 真实采集：runtimeSidecar 旁路（有界通道不阻塞状态机）、日志按 (pod,container,restart) 流键跟随、每观察间隔每流 ≤1 块、restart 变化自动换流；指标复用 kube.MetricsCollector 同 startup counter 差值、首样本/reset/缺测跳过、按 Pod 严格递增毫秒序号；MetricsEnabled 默认 false 失败关闭（未配 Kubelet 凭据零发射）。三方契约（流键/Seq/单块上限/字段语义）已核对一致；真实 Kubelet 精度、断网重传、日志跨界分块留待实机。
- wave 2 收尾验证：E/D/F 三方交付后全平台 `go test ./... -race -count=1`（真实 PostgreSQL/Redis）37 包全绿零失败；前端 31/31 测试与生产构建通过。首期代码面（P1-P5 本地可验证部分）至此完整；剩余见"仍未完成"。
- G 期完成域名 TLS 引用纵切：迁移 018（application_domain_claims 加 tls_secret_name 列）、实例管理员 Set/GetDomainTLSSecret API（DNS1123 校验、审计）、worker/deployment 协议加 https+tls_secret_name、ClaimPending 领取时汇聚——全部冻结域名绑定同一非空 Secret 才下发 HTTPS，缺绑或不一致失败关闭（不猜证书）；绑定是实例级运行设施，取当前值不冻结进 release，证书轮换改绑定即对下次发布生效。真实 Traefik/ACME 与真实集群挂载留待实机。
- G 期发现并由主会话修复既有缺陷：WORKER 类型应用首次发布因空域名 slice→JSONB NULL 违反 application_releases.domains NOT NULL（23502），既有测试从未覆盖 WORKER 发布；已显式落空数组并补 TestWorkerFirstDeploymentPersistsEmptyDomains 回归测试。
- H 期完成源码过期清理：source.Service.Cleanup(ctx, now, limit)——过期 PENDING/UPLOADED 上传删文件删行；过期快照按引用分类（无引用成对删快照+COMPLETE 上传行、仅终态构建引用删文件置 EXPIRED 墓碑、非终态构建引用绝不删且不占配额）；先文件后行、os.Root 防逃逸、单条失败不中断。审计决策：audit_events.actor_id 有 users 外键，系统操作不伪造 actor，不写审计、计数返回给接线方。主会话已接线 lifecycle 清理循环（10 分钟节奏，limit=100）。server/source 集成回归绿。
- I 期完成 OAuth 预注册客户端管理 Connect API（OAuthAdminService：Register/List/SetActive，实例管理员专属，复用既有 CIMD/停用即撤权语义，键集分页游标 scope 绑定，重复 client_id 权限先行预检→Aborted，无任何 secret 字段）；主会话已挂入 server /api 路由（oauthService 未装配时不注册，失败关闭）。server/oauth 回归绿。F2 期完成全局管理页「OAuth 客户端」区块（列表键集分页、注册表单、启停二次确认含"停用即撤销全部授权且重启不恢复"警示、非管理员门控沿用）；web 38 用例与构建通过、零硬编码色值、零新依赖。浏览器同意流+CLI 登录端到端联调留待实机。
- G/H/I 三期整合后全平台 `go test ./... -race -count=1`（真实 PostgreSQL/Redis）37 包全绿零失败。
- J 期完成本地 stdio MCP 业务工具桥接：NewBridgeServer 以 clientcli 已认证客户端为后端暴露 19 个工具（工作空间/应用/构建/部署/运行日志指标/生命周期），工具名与入参 schema 与远程 HTTP MCP 同形（本地 PAT 全局视角增加 workspaceId/applicationId 定位字段，缺省回退 profile 默认）；安全边界（密钥与敏感配置值不回显、secretRefs 仅引用、日志游标分页、PAT 只进请求头）；未登录保持仅 source_preview 失败关闭。真实编辑器 stdio 联调与长会话 token 自动续期未做（已知权衡：过期需重启 MCP）。F2 期同步完成全局管理页 OAuth 客户端区块。整合后全平台 37 包回归再次全绿；web 38 用例全绿。
- 真实后端 e2e 首次跑通：本机起真实控制面（真实 yummy_e2e 库迁移 + 真实首位管理员初始化 + 真实 Redis 会话 + 嵌入式 Web UI，非 mock 服务），Playwright 5/5 通过（登录页四档响应式 + 真实登录/个人账户/退出/未登录重定向）。验证范围仍限本机 Mac 与真实 Web/身份链路——不含 k3s/构建/HTTPS 实机验收。e2e 跨页流程扩展（全局管理/OAuth 客户端/空间应用创建/运行配置保存/PAT）进行中。
- 真实联调立即发现并修复两个缺陷（正是真实后端 e2e 的价值）：①前端用户/应用状态比较与提交全部用小写 "active/disabled"，后端常量为大写 "ACTIVE/DISABLED" 且服务端直接校验——用户管理页面对真实后端整体失效（徽章颠倒、停用/启用/重置密码不可用）；已统一 platform/account/overview/workspaces 四页比较与提交值并同步测试夹具，web 38 用例复绿。②e2e 实例首次启动漏配 --oauth-key，OAuthAdmin 按设计失败关闭未挂载（e2e 子智能体以"mux 404 而非 Connect 错误格式"精确诊断）；已带 oauth-key 重启并验证路由返回 Connect 格式响应。
- 真实后端 e2e 扩展完成（admin.spec + workspace-app.spec + 既有 console.spec 共 8 用例两轮全绿，复跑稳定）：全局管理七页签真实切换、OAuth 客户端全生命周期（注册/停用/重启用/再停用+刷新持久化复核）、创建空间→名称-only 建应用→运行配置真实保存 v1→构建资格前置如实断言（绝不断言构建/部署成功）→PAT 全生命周期（明文恰好出现一次、撤销持久化）。真实联调再发现并修复两个缺陷：③OAuth 注册 scopes 留空 → proto3 nil slice → JSONB NULL 23502（与 WORKER 域名同类缺陷，前端提示"可留空"）；已在 RegisterClient 显式归一化空数组。④全新实例 instance_settings 单例行由实例管理操作懒创建，此前 ResolveExecution 对无行实例返回通用"资源不存在"误导为无权限；已改为具体前置文案"实例尚未配置执行策略"并同步 e2e 断言。一次重启后首轮 OAuth 用例偶发失败未复现（此后两轮 8/8，服务端日志无对应错误），如实记录。
- worker mTLS 注册真实冒烟通过（本地可验证的登记链路终点）：开发 CA + 服务端证书启动 worker HTTPS 监听 → 真实管理 API 创建执行节点（readiness 如实显示"等待真实节点资格检查"）→ 签发一次性注册码 → `platform worker register` 真实 ECDSA P256 密钥+CSR 经 mTLS 换取 CA 签发证书（30 天有效/7 天轮换窗）→ state 目录 0700/文件 0600 → 重放同一注册码被 401 拒绝 → 证书在 worker_certificates 登记且未撤销。worker run 需真实 k3s kube-apiserver，本地不做也不伪造；节点资格仍失败关闭。
- K 期完成 OAuth 授权码+PKCE 全协议流 e2e（9 用例，全量 18 用例）：元数据发现、真实 consent UI + PKCE S256 全流（state/iss 回显、code 交换、Bearer 访问受保护资源）、匿名跳登录后回归授权流、刷新轮换（旧 access 有效期内可用、重用 refresh 撤销整个授权族）、撤销连带失效、四个负向分支。真实发现并修复两个缺陷：⑤缺陷A（阻断级）consent UI 用 FormData(multipart) 提交而服务端 Consent 仅 ParseForm(urlencoded) → CSRF 读不到 → 真实浏览器无法完成任何 OAuth 授权；已改 ParseMultipartForm 兼容两种编码，并新增"真实 UI 按钮提交"回归用例验证 multipart 路径。⑥缺陷B 未知 client_id 的 /oauth/authorize 以 nil 请求调 fosite WriteAuthorizeError → panic → 500；已改为 writeAuthorizeFailure 直接返回标准 400 invalid_client/invalid_request（不向未核实回调重定向）。运维特征如实记录：整套 e2e 每轮约 14-18 次登录，贴近 20 次/15 分钟频控预算，连跑需清理 yummy:login:* 键或等待窗口；曾出现一轮进程路由态残留（mux 404）未复现，干净重启后消失。e2e 用例已加固：跨运行数据累积下按分页逐页定位目标行、无分页控件页面容错、scripts/reset-e2e.sh 一键重置实例数据。
- 产品冒烟落地：旧 uppercase demo 冒烟保留为 scripts/smoke-demo.sh（make smoke-demo），新增 scripts/smoke-product.sh（make smoke）——对 bin/platform 真实验证迁移→首位管理员→启动→登录识别管理员→创建工作空间持久化→OAuth 元数据，使用临时库 yummy_smoke 用完即弃（曾修复 transfer 端口与默认实例冲突、补 oauth-key 使 OAuth 元数据挂载纳入冒烟）。
- K2 期完成 e2e 第三轮（source-upload + member-permission，全量 20 用例两轮全绿）：源码安全上传全流（合法包 READY/SHA/大小/条目与本地独立计算比对；`../evil.txt` 路径逃逸与 `.env` 敏感文件被服务端真实拒绝且原文案断言）、邀请全流（一次性令牌、短密码服务端拒绝、二次消费拒绝、新成员权限门控按 access.go 真实语义断言、管理员授予"开发者"后可见性变化）。真实发现：①【高·部署契约】源码上传前端同源 fetch `/transfer/`，而传输入口独立监听 --transfer-addr 且强制 Host==PublicBaseURL——本地无反代必然 404，生产部署必须由反向代理把 /transfer/ 转发到 transfer-addr 并保留 Host 头（与 2026-11-10 记录的反代契约一致）；本地 e2e 以等价 route 转发验证协议全真实。②【低】接受邀请页密码前端只校验非空（hint 与服务端 12-128 字节校验不一致）——已由主会话统一为 passwordSchema。③既有 findRow 在列表跨 24 条后存在翻页时序竞态，K2 以归档历史 e2e 空间/清理历史客户端治理数据后稳定；后续轮次数据接近阈值时需同样治理或引入删除类 RPC。
- P6 运维文档落地：docs/operations.md（构建依赖/控制面安装与全参数表/worker 登记运行与资格流程/反代与 HTTPS 契约含 /transfer/ 转发与 Host 保留/数据保留配额/升级顺序与备份三件套（PG+source-root+master-key）/验证状态与限制边界）。全部数值（20MiB 构建日志、16KiB 块、2000 块与 4MiB 运行日志配额、24h 指标、10 分钟清理、20 次/15 分钟频控）已逐项对照代码核实。
- 2026-11-11 交接快照（会话移交，后续可由其它工具接续）：
  - 工作区状态：`main`，**全部产品工作未提交**（未跟踪的 internal/ api/ gen/ web/ migrations/ docs/ scripts/ deploy/ 与已跟踪文件的修改就是产品本体；另有已暂存的 smoke.sh→smoke-demo.sh 重命名与 internal/webui/dist 构建产物更替）。接手者勿 reset/clean。
  - 本日新增：worker 证书自动轮换（workerexec/rotate.go：RotateCertificate 原子换发 + RotationManager 定时器 + TLS GetClientCertificate holder 回填 + run 装配，测试含轮换/失败保留/权限/泄漏断言；修复一处测试侧内存状态回写竞态——实现本身稳健）；独立只读审查产出验收矩阵（P1-P4 逐项 file:line 核实通过、P5 通过、P0/P6 部分缺口即实机项；安全不变量四类抽查全过；运维文档数值零偏差；报告全文在会话记录）；oauth_requests 过期清理（CleanupExpiredRequests + lifecycle 接线 + 幂等/边界测试）；计划文档各阶段状态指针。
  - 交接时验证快照：Go 全平台 `-race`（真实 PG/Redis）37 包全绿零失败；web vitest 38/38 + 构建通过；真实控制面 Playwright e2e 20/20（频控键清完后单轮）。
  - 本机运行实例：e2e 控制面（127.0.0.1:8080/8081/8443，库 yummy_e2e，管理员 e2eadmin/e2e-admin-password，证书 var/e2e/certs）——会话交接前停止，重启命令见 var/e2e/keys.env 说明与 docs/operations.md §2-3。
  - 接手操作要点：Go 工具链 `export PATH="$HOME/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.darwin-arm64/bin:$PATH"`（如遇构建缓存权限再加 GOCACHE 覆盖）；Node 用 `export PATH="$PWD/work/node/bin:$PATH"`；测试需 TEST_DATABASE_URL/TEST_REDIS_ADDR（本机 docker 容器 yummy-postgres-1/yummy-redis-1）；e2e 连跑前清理频控键 `yummy:login:*` 或用 scripts/reset-e2e.sh 重置实例。
  - 待办（均需外部条件或明确指令）：实机验收（专用 Linux/AMD64 服务器：k3s+BuildKit 构建推拉/真实域名 HTTPS/Kubelet 指标/断网重传/PVC 与 CNI 专项）并出具验收矩阵；是否提交 git 由负责人决定（当前有意保持未提交）。
- 主会话实现证书轮换时构建授权恢复（此前列为待设计项）：OpenBuildGrant 下载验证从"签发序列号仍有效"放宽为"该节点当前存在任一有效证书"（节点粒度信任边界——下载端本就是受限 Bearer；轮换透明不断在途构建，节点全部证书撤销才阻断）；GrantForBuild 在 attempt 尚无确认 JobUID（旧令牌不可能被任何 Kubernetes Secret 引用）时原子换绑当前序列号重新发放，JobUID 已存在绝不换发。不需要重建 Job 或更新 Secret。原"撤销 a1 阻断下载"断言在节点无其它有效证书时语义不变（向后兼容），新增 TestBuildGrantSurvivesCertificateRotation 覆盖换绑/不换发/节点粒度放行/全撤销阻断四个分支。
- 仍未完成：OAuth 客户端管理 API、stdio MCP 完整业务工具桥接、源码文件自动清理、证书轮换时的构建授权恢复、Playwright 真实后端扩展用例、E/F 期收尾与联调、真实 Linux/AMD64 k3s/BuildKit/registry/DNS/HTTPS 实机验收。

## 已确认决定与外部环境

- 2026-11-07 用户确认：实例管理员自动拥有全部工作空间应用业务访问与操作权，包含源码/日志/发布/回滚；仍不得管理他人的个人 PAT/Agent 连接，业务操作记录审计。
- 用户选择先完成本地开发和自动测试，稍后提供专用服务器用于真实验收。当前不扫描或修改既有集群/仓库。
- 凭证撤销/自然到期只阻止后续请求，不隐式取消已开始的构建；账号停用取消排队构建并请求停止正在执行的构建，已上线应用不自动关闭。
- 发布失败自动恢复默认关闭，作为显式实例策略配置；手动恢复/回滚始终要求权限与版本检查。
- 真实 Linux/AMD64 节点、固定 k3s/BuildKit 版本、可推拉 OCI HTTPS 仓库、可配置 DNS 的根域名和证书方式尚未提供。未配置前的本地/模拟集群测试不算 AMD64 原生资格或公网 HTTPS 验收。

## 2026-11-13/14 实机验收记录（tencent-sh，上海，公网 122.51.59.201）

环境：Ubuntu 24.04 / 2C3.6G / k3s v1.34.5（单节点，存量 traefik+metrics-server）/ 宿主 rootless BuildKit 常驻守护（专用用户 yummy-builder + systemd user 服务）/ 私有 registry:2（自签 CA + htpasswd，NodePort 30500，安全组仅放行 80/443）/ PostgreSQL+Redis 以 pod 跑在 yummy-infra 命名空间。产品二进制交叉编译 linux/amd64 部署为宿主 systemd 进程；Tailscale/docker(derper)/存量集群配置全程零改动（只读避开）。

### 逐项验收矩阵（真实结果，未验证项如实标注）

| # | 验收项 | 结果 | 证据 |
|---|---|---|---|
| 1 | 一次性注册码 mTLS 登记（真实 ECDSA P256 CSR→worker-ca 签发 30 天/7 天轮换窗） | ✅通过 | worker-state 四文件 0700/0600；证书登记 serial 28331090ec663b25479f5b350b97561dba94e4e4 |
| 2 | 注册码重放拒绝 | ✅通过 | 二次使用 HTTP 401 |
| 3 | state/journal 目录权限失败关闭 | ✅通过 | 0755 的 state-dir 与 journal 均被真实拒绝（unbound/权限检查生效） |
| 4 | 节点资格链路（实例管理员登记+证据→节点就绪） | ✅通过 | GetNode：ACTIVE，build/runtime/ingress/metrics 全 true，readiness="已核验登记资格证据" |
| 5 | 固定镜像 digest 钉住失败关闭 | ✅通过 | worker 拒绝非 digest 引用（node.go pinnedDigest） |
| 6 | 输出仓库凭据验证链路（新增 VerifyRegistry RPC） | ✅通过 | ok/net/auth/read 全 true，verified_at 落库；失败（错误凭据/引用不存在）不标记（回归测试） |
| 7 | 真实构建推拉部署闭环——Go HTTP | ✅通过 | 真实源码上传（/transfer/ 反代链路+SHA 校验）→ 宿主 rootless BuildKit 构建 → 推私有仓库 digest 365ae06b → 部署 SUCCEEDED → Pod 运行私有仓库镜像 → 公网 HTTPS 200（"go-http-demo @ ya-823162ef... up ..."） |
| 8 | 真实构建推拉部署闭环——Node HTTP | ✅通过 | bld_276d4277 SUCCEEDED，node:22-alpine 真实拉取构建，Pod Running，公网 HTTPS 200（"node-http-demo up 413s"） |
| 9 | 真实构建推拉部署闭环——静态站点 | ✅通过（应用侧换 nginx-unprivileged 镜像后） | bld_a757669a SUCCEEDED，公网 HTTPS 200 返回完整 HTML；官方 nginx 镜像与产品固定 uid 1000 不兼容（发现⑥，属用户镜像选择问题，如实记录） |
| 10 | 真实构建推拉部署闭环——后台进程（WORKER 型） | ✅通过 | ⑦修复后：构建 SUCCEEDED → 部署 op SUCCEEDED（dop_55d17a14）→ Pod Running（worker-bg tick 日志真实输出）。隔离实验证实此前不收敛系 correctImageID 误判（⑦），多次测试重叠的跨应用 attempt 污染为触发背景 |
| 11 | 公网入口 TLS（自签证书+Host 校验+反代契约） | ✅通过 | Traefik 终止 TLS 下发验收 CA 证书（SNI 匹配）；/transfer/ 经真实反代转发+Host 保留；127.0.0.1 直连被反 DNS 重绑定保护拒绝（设计行为） |
| 12 | 运行日志/指标链路 | ✅通过 | ⑨修复后：runtime_log_chunks 65/16 块真实 tick 日志入库（两个应用）、runtime_metric_samples 92+ 采样持续产出；GetRuntimeLogs/GetRuntimeMetrics 链路随 DB 数据打通 |
| 13 | Kubelet 指标精度与 webhook 授权 | ✅通过 | ①SA token 直测 kubelet /metrics/resource 返回 200（nodes/metrics RBAC webhook 授权实证）；②采集值与 kubectl top 同源一致（cpu 0≈1m、内存 0.39Mi≈0Mi）；③采样序列严格递增、CPU counter 差值窗口真实（峰值 1113m 突发 / 8m 稳态如实呈现） |
| 14 | 断网重传/journal 事件去重 | ✅通过 | 真实构建进行中 kill worker（QUEUED 窗口）→ 控制面保留非终态 → 重启后 journal 重放 RECONCILE → 构建重跑收敛 SUCCEEDED；事件序列干净（attempt seq 1 ACCEPTED → 2 SUCCEEDED，无重复无乱序）。另附多轮 worker 重启+断连的 journal 重放均正常恢复（本验收全程 15+ 次重启） |
| 15 | PVC 保留 | ✅通过 | 带卷应用（1Gi local-path PVC）写入 6 行 → 删 pod（RS 重建挂同一 PVC）→ 新 pod 数据完整保留（46 行，首行 line-1 时间戳不变） |
| 16 | NetworkPolicy CNI 执行 | ✅通过 | 按新约定（只放行同 ns+Traefik）应用默认策略后：①Traefik 公网入口不受影响（A2 HTTPS 200）②同命名空间放行（app 响应正常）③跨命名空间（yummy-infra→app）被拒（差分证明 CNI 强制生效）；Tailscale 回归：ping apeng-ubuntu 正常（DERP 中继），宿主与集群零影响；出站回归：pod 公网访问与镜像拉取正常 |
| 17 | 全量回归（真实 PG/Redis，-race） | ✅通过 | 两轮 EXIT=0 零失败（含全部实机修复）；宿主构建路径相关四包（workerexec/buildrunner/kube/cli）专项回归绿 |

### 实机发现与修复（全部真实缺陷，均有回归测试或部署验证）

| # | 缺陷 | 级别 | 修复 |
|---|---|---|---|
| ① | 文档化生产部署（§4 反代终止 TLS）下 CSRF 全部 403：gorilla/csrf 的"必须TLS"检查在应用层必然误报（e2e 因 allow-local-http 走豁免分支从未暴露） | 阻断 | 新增 --behind-tls-proxy（仅豁免该检查；Cookie Secure/Host/Origin 校验不变）+ 回归测试 |
| ② | registry verified_at 无任何写入路径 → 构建闭环死锁（fixture 伪造掩盖） | 阻断 | 新增实例管理员 VerifyRegistry RPC（真实探测+失败关闭标记）+ 测试 |
| ③ | kubelet 挂载 tmpfs 为 root 属主 1777，fsGroup 不改属主 → buildrunner 对挂载根 chmod 必然 EPERM，构建 100% 失败 | 阻断 | ensureOwnedRuntime：在挂载内创建属主为当前用户的私有运行目录 + 测试 |
| ④ | 架构级：k8s pod 内嵌套 rootless 被 containerd 锁定挂载+默认 seccomp 双重封锁（privileged 方案违背安全不变量）；官方 rootless-in-k8s 文档实际要求 privileged pod | 架构 | 经负责人确认改为方向 D：构建由 worker 在宿主直接驱动节点级 rootless BuildKit 守护（新增 host_build 资格位 + workerexec/hostbuild.go + --buildkit-addr/--buildctl-path）；部署仍走 k8s pod |
| ⑤ | dockerConfigJSON 用 origin.Hostname()（丢端口）作 auths 键 → 带端口私有仓库推送 401 | 阻断 | 改 origin.Host（host:port 完整形态） |
| ⑥ | 固定 uid 1000 与官方 nginx 镜像不兼容（需 root 建 cache） | 兼容性 | 属用户镜像选择问题；验收改用 nginx-unprivileged 变体通过。如实记录，产品不做放宽 |
| ⑦ | WORKER 型部署观察不收敛 | **已修复** | 根因：内容寻址构建使不同应用的镜像 digest 相同，containerd 的 ImageID 引用路径取决于先拉取者，correctImageID 整串精确比较误判同 digest 合法证据。修复为摘要等价比较（runtime.go correctImageID + 观察原因透传诊断）。修复后 WORKER 型部署 op SUCCEEDED、pod Running（2026-11-14） |
| ⑧ | worker 客户端把非资格类业务拒绝（channel=deployment）当协议错误断流 → 归档应用重投形成"拒绝→断流→重连"风暴（359次/10分钟），节点心跳断开连带资格失效 | 阻断 | 修复：业务拒绝上报后保留流（workergateway/worker_client.go Rejection 分支）。归档应用的存量重投以 DB 标记终态清噪（测试实例数据操作） |
| ⑨ | 运行日志/指标采集产出为 0 | **已修复（三处叠加根因）** | ①采集生命周期错配：旁路挂操作跟踪窗口，⑦修复后收敛只需数秒，旁路来不及产出 → 改挂部署本体（collectors.go 持久采集器，Apply 启动/LOST 或 STOP 停止）；②分块阈值 16KiB 对低频日志永远凑不满且无时间刷出 → 按观察间隔时间刷出半满 buffer + 取消/EOF 时同步尾块；③CLI 从未接线 MetricsEnabled/KubeletMetrics（worker run 新增 --kubelet-metrics-endpoint）|

### 安装侧要点（非产品代码，实机参数）

- k3s config.yaml 曾有未填模板占位符 `<你的 tailscale IP>` → 已填 100.120.208.65（否则任何证书重签即启动循环失败）
- k3s 1.34 从 registries.yaml 生成 certs.d hosts.toml 时丢弃 auth → 需手工恢复（k3s 重启后需复查）
- containerd certs.d 目录名必须为字面 host（含冒号），ctr CLI 还需 --hosts-dir 或 /etc/containerd/certs.d 符号链接
- ctr push 的 TLS 行为异常（显式 hosts-dir/CA 均无效）→ 验收用 curl 手工实现 OCI 推送（03b 脚本，digest 一致性校验）
- buildkitd 需烧入 docker.io mirror 配置（不经 containerd 加速）；runner 镜像需对齐 /usr/local/bin/buildctl-daemonless.sh 契约路径
- 宿主 rootless buildkitd 需要：uidmap 包（newuidmap）、sysctl apparmor_restrict_unprivileged_userns=0、专用用户+linger、socket 落用户自有目录
