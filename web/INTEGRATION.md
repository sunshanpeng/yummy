# 正式 React 控制台联调说明

## 本轮边界

仅修改 `web/`，不编辑 `web/src/gen/` 或后端。正式入口为 `index.html`，旧 uppercase 演示入口已移除。

已接线页面均调用真实生成描述和 Connect API，生产代码没有 mock 数据、模拟成功定时器或演示部署。测试中的传输响应替身仅位于 `.test.tsx/.test.ts`。

## 请求与身份

1. 首次 RPC 之前执行同源 `GET /auth/csrf`，读取 `{csrfToken}` 并让浏览器保存 CSRF Cookie。
2. 生成描述来自 `src/gen/platform/v1/*_pb.ts`，通过 Connect `createClient` 创建客户端；`useHttpGet:false`，包括 GetSession 在内的 Connect POST 均添加 `X-CSRF-Token`。
3. 所有请求 `credentials:same-origin`，认证仅使用 Cookie。CSRF 令牌只保存在内存；PAT、邀请和重置令牌不写 localStorage/sessionStorage/Query mutation 缓存。
4. GetSession 的 user 可为空，匿名路由到登录；有 user 时使用 `id/authVersion` 分区缓存。会话过期清理请求与缓存并返回登录；退出成功也清理。
5. Query 通过官方 Connect Query `createQueryOptions` 适配，外层 key 包含身份、authVersion、界面作用域，内层保持生成请求键（含 workspace/application/object IDs）。切域取消并移除旧域请求；写操作按生成方法名失效相关查询，不乐观宣告操作成功。
6. 所有写操作禁用自动重试。创建应用、运行配置保存、构建创建/重试、部署均在相同冻结输入、失败重试时复用幂等键，输入变化生成新键。修改构建预览输入会清除旧预览，必须重新检查。

## 路由与接口

| 范围 | 路由 | 已接真实接口 |
|---|---|---|
| 本地身份 | `/login`、`/accept-invite`、`/reset-password` | GetSession、Login、Logout、AcceptInvitation、CompletePasswordReset |
| 个人账户 | `/account` | ChangePassword；用户名只读 |
| 个人 PAT | `/account/tokens` | List/Create/RevokeAccessToken；自己所有、应用至少一项、scope 按已选应用能力交集选择、有效期 7/30/90 天、明文仅组件内一次展示 |
| 空间选择 | `/`、空间菜单 | ListWorkspaces、GetWorkspace |
| 空间日常 | `/w/:workspaceId/apps` | ListApplications、CreateApplication（只填名称，当前空间与幂等键）；创建后进本应用运行配置 |
| 应用详情 | `/w/:workspaceId/apps/:applicationId` | GetApplication、ListBuilds、ListDeployments、GetRuntimeStatus；构建结果/发布结果/当前运行观察分栏，不由其中一个推导另一个 |
| 应用资料 | `…/settings/basic` | UpdateApplication（expectedRevision） |
| 应用授权 | `…/settings/access` | List/Set/RemoveApplicationMember；候选来自空间成员，继承权限不能在应用中移除 |
| 运行配置 | `…/settings/runtime` | Get/SaveRuntimeConfig；type/port/probe/resources/command/args/domains、变量/文件/卷、confirmed、expectedRevision；敏感值空回显，显式 keepExisting |
| 源码 | `…/source` | CreateSourceUpload → 同源 PUT transferPath → CompleteSourceUpload；READY 快照与已知 ID 的 GetSourceSnapshot/原包下载；不是构建 |
| 空间管理 | `/w/:workspaceId/manage/settings`、`members`、`invitations` | UpdateWorkspace、List/ChangeRole/RemoveWorkspaceMember、Create/List/RevokeInvitation；应用授权只概览和跳转 |
| 全局身份 | `/platform/workspaces`、`users` | Create/ListWorkspaces、ListUsers、SetUserStatus、CreatePasswordReset；绝无他人 PAT 管理 |
| 全局资源 | `/platform/nodes`、`registries` | List/Create/UpdateNode、List/Create/UpdateRegistry；不伪造资格，不通过普通更新启用 DISABLED 节点；凭据未替换完全省略 optional 字段 |
| 全局策略 | `/platform/policy`、`allocations` | Get/SaveExecutionPolicy、Get/SetWorkspaceAllocation；NotFound 显示首次配置，expectedRevision=0，不冒充已有配置 |
| 构建设置/密钥 | `…/settings/build` | Get/SaveBuildConfig；List/Create/Replace/DeactivateBuildSecret，仅 secret:write 展示应用密钥元数据，明文只写 |
| 构建 | `…/builds`、`new`、`:buildId` | ResolveExecution(0) → PreviewBuild → 二次确认 CreateBuild / RetryBuild；List/GetBuild、ReadBuildLogs 增量纯文本、CancelBuild 等待实际回执 |
| 发布与运行 | `…/deployments`、`:operationId` | List/GetDeployment、GetRuntimeStatus、检查冻结后 CreateDeployment、RollbackDeployment、ChangeRuntime(START/STOP/RESTART)；操作受理不等于生效 |
| 节点注册 | 全局 `/platform/nodes` | CreateNodeRegistration 一次性明文、Disable/EnableExecutionNode、RevokeWorkerCertificate；重新启用需重新注册 |

资源/空间分页统一使用后端 cursor，每页 24 条，URL 保留主列表 cursor；没有全局构建或 Project 层。身份接口当前没有 cursor（用户/邀请/PAT），前端不伪造服务端分页。资源选择器也提供分页，不把第一页当完整全集。

工作空间与应用业务按钮只读取后端 capabilities；全局页入口与 guard 使用后端 GetSession 的 instanceAdmin。空间角色文案只用于显示，不复制策略规则。空间管理不出现节点/仓库/平台策略；account/platform 不出现空间切换。

## 运行配置和源码注意事项

- revision=0 的运行配置是后端系统建议，尚未保存。保存只生成配置版本，不表示已部署；未确认可保存草稿。
- HTTP 与后台进程分别提交有效字段，后台进程提交 port=0、无探针路径和域名。
- 敏感变量和文件使用后端 configured 标记；未替换发送 keepExisting=true、value/content 为空，不发送掩码。成功后清除输入中的秘密。
- 文件与卷最终的重叠路径、安全路径、资源、域名及规模限制由后端独立校验，错误原位展示且保留输入。
- 源码浏览器摘要使用 Web Crypto SHA-256，需要 HTTPS 或 localhost 安全上下文；最大 256 MiB，并仍受实例更低限制约束。控制面负责 tar 检查；前端不解压或执行文件。
- 上传会话与传输/完成分别反馈，完成失败后保留会话可重试 Complete。没有模拟百分比或假任务。READY 快照可跳转当前应用创建构建。
- 构建以只读业务 ResolveExecution(expectedPolicyRevision=0) 取得当前策略，Preview/Create/Retry 均冻结明确非零策略版本。普通用户不会调用实例管理员 GetExecutionPolicy。预览返回的架构、节点与镜像路径只读。
- 发布写入的 ExpectedRuntime 同时包含最新 generation、当前 servingReleaseId（无则空）与**最新已保存草稿** configRevision。回滚使用目标 Release 的旧镜像/旧配置，但 Expected.configRevision 仍保护最新草稿，绝不回退数据库和卷内容。
- 构建日志只以服务端 seq 增量读取，保留最近 500 段上限；切换构建中止旧请求，不执行 HTML/ANSI。运行日志与构建日志不是同一接口。

## 本地代理与正式部署

前端仅为 Vite SPA，不新增 Node SSR 或生产常驻服务。开发代理配置在 `vite.config.ts`：

- `/api`、`/auth` → `VITE_CONTROL_PROXY`（默认 `http://127.0.0.1:8080`），changeOrigin=true。
- `/transfer` → `VITE_TRANSFER_PROXY`（默认 `http://127.0.0.1:8081`），Host 显式与 `VITE_CONTROL_PROXY` authority 一致，使会话校验对应 PublicBaseURL。
- 实例的 PublicBaseURL、请求 Origin/Host、Cookie Secure/Domain 与反向代理策略必须一致；正式同源反代将 `/transfer` 转发 transfer listener，其他页面/API 转控制面。控制面需配置 source root 和 runtime master key，由父级实施。
- SPA 静态托管需对非资源路径回退 `index.html`，同时不能吞掉 `/api`、`/auth`、`/transfer`。

本轮没有自行启动服务，也没有截图交付。

## 验证

在 `web/` 工作目录使用 Node 24：

```bash
PATH="../work/node/bin:$PATH" pnpm test --run
PATH="../work/node/bin:$PATH" pnpm build
PATH="../work/node/bin:$PATH" pnpm test:e2e --list
```

当前 23 个 Vitest 用例通过，包含旧 demo、CSRF 顺序/Cookie/POST/不重放/上传安全、表单验证、服务端 cursor、一次性明文不入存储或缓存、独立账户域/全局 guard、应用幂等重试、敏感 keepExisting、仓库 optional 凭据和首次策略空态，以及构建预览冻结/输入变更重检/Retry 完整输入/纯文本日志与序号/部署陈旧草稿拒绝/CAS 与幂等重试/构建密钥权限。生产构建成功，平台/runtime/source/build/deployment/overview 按路由分包，无大于 500kB 的入口警告。

Playwright 配置只连接现成服务，没有 webServer。已收集 5 项：375/768/1024/1440 登录页横向溢出与键盘焦点、真实登录→个人/PAT 域→退出。未冒充完成真实浏览器验证；父级启动集成服务后执行：

```bash
YUMMY_E2E_BASE_URL=http://127.0.0.1:8080 \
YUMMY_E2E_USERNAME='<真实测试用户名>' \
YUMMY_E2E_PASSWORD='<真实测试密码>' \
PATH="../work/node/bin:$PATH" pnpm test:e2e
```

不配置截图、video、trace，不把登录凭证产物留作交付。

## 尚未接线

- 运行日志、CPU/内存监控读取、域名/HTTPS 实际探测与生效仍依赖对应后端模块；不展示假成功或假图表。构建/发布已有真实页面不等于真实 BuildKit/k3s/Ingress 生产验收完成。
- OAuth/Agent 授权流、审计与仓库真实 probe/推送资格动作，待后端对应协议逐步补充。
- 完整后端联调、真实浏览器各业务页面响应式、读屏与 WCAG AA 审核仍需父级的运行服务与测试身份，不以 Vitest 替代。
