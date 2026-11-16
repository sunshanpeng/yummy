# 命名规范、集群/环境/部署与最小 CLI

日期：2026-10-26。状态：负责人已确认的设计决定，尚未实现。本文是这些规则的唯一细则；[产品方案](2026-10-22-agent-first-build-product-design.md)、[技术方案](2026-10-22-agent-first-build-technical-design.md)、[首期运行方案](2026-10-23-paas-v1-runtime-design.md)、[前端方案](2026-10-22-agent-first-build-frontend-design.md)、[创建与配置边界](2026-10-24-application-configuration-boundaries-design.md)、[交付方式](2026-10-24-delivery-model-and-instance-boundaries-design.md)和 [OAuth 方案](2026-10-23-oauth-mcp-auth-design.md)的正文已同步修改。

## 1. 决定摘要

| 主题 | 决定 |
|---|---|
| 名称 | 应用名与工作空间的空间名使用同一命名规范，创建时校验，创建后不能修改；工作空间另有可修改的显示名称 |
| 默认地址 | `<应用名>-<6 位后缀>.<应用根域名>`，后缀在创建应用时生成且不变；地址里不含工作空间和部署 |
| 应用根域名 | 与控制台分属不同的可注册域名 |
| 运行位置 | 平台管理 k3s 集群，不管理机器。第一版每个实例一个集群、一个默认环境 `prod`，每个应用一个默认部署 `default`，界面不显示环境和部署 |
| 网络隔离 | 只隔离工作空间：运行命名空间只接受同命名空间与 Traefik 的入站流量，其他规则以后按需添加 |
| CLI | 第一版只做最小命令，用 PAT 登录；浏览器 OAuth 与远程 HTTP MCP 放到第一版之后 |

## 2. 命名规范

应用名和空间名遵守同一规范：

- 只用小写字母、数字和中划线 `-`；
- 以字母开头，以字母或数字结尾；
- 最长 30 位；
- 不能有连续的 `--`。

校验用正则 `^[a-z](-?[a-z0-9])*$`，另外检查长度不超过 30。这个正则没有前瞻语法，Go 的 regexp、浏览器和 Zod 可以共用。页面只做提前提示，输入框失焦时就地显示错误；API、CLI、MCP 创建时都由后端校验，错误码为 `NAME_INVALID`（不符合规范）和 `NAME_TAKEN`（重名）。

| 名称 | 唯一范围 | 能否修改 | 用途 |
|---|---|---|---|
| 空间名 | 实例内 | 不能 | 运行与构建命名空间、CLI 中的空间标识 |
| 空间显示名称 | 不要求唯一 | 可以 | 只用于界面，最长 40 个字符，可含中文和空格；留空时显示空间名 |
| 应用名 | 空间内 | 不能 | 默认地址、K8s 资源名、CLI 中的应用标识 |

工作空间创建时填写空间名（必填）和显示名称（可选），由全局管理员创建；之后空间设置只能改显示名称与说明。

归档的空间和应用继续占用名称。删除应用时，名称要等它的 K8s 资源和保留的卷清理完才能复用；worker 按 workspace_id、application_id 标签核对归属，不接管旧对象（见第 5 节）。

## 3. 默认访问地址

格式为 `<应用名>-<后缀>.<应用根域名>`，例如 `gateway-api-k7m2x9.example.net`。

- **后缀**：创建应用时随机生成 6 位小写字母和数字，写入 `applications.host_suffix`，之后不变。`<应用名>-<后缀>` 在实例内有唯一约束，冲突时重新生成。
- **不含空间和部署**：应用名只在空间内唯一，实例内唯一由后缀保证。地址标签最长 30 + 1 + 6 = 37 位，在 DNS 单段 63 位以内。
- **证书与 DNS**：默认地址只有一层子域名，一条 `*.<应用根域名>` 泛解析即可覆盖全部应用。证书可以逐个用 ACME HTTP-01 签发，也可以由管理员预置一张 `*.<应用根域名>` 泛域名证书。
- **合法性**：应用名符合命名规范，接上 `-` 和后缀后不会出现 `--`，也不会出现第 3、4 位连续中划线这种证书机构拒签的标签。
- **与控制台分开**：应用根域名由全局管理员配置，必须与控制台 `PUBLIC_BASE_URL` 分属不同的可注册域名，例如控制台 `build.example.com`、应用 `example.net`。保存时按公共后缀列表判断（Go 可用 `golang.org/x/net/publicsuffix`），同属一个可注册域名时拒绝保存。全托管时把应用根域名提交公共后缀列表，让不同用户的应用之间也跨站。
- **修改根域名**：只影响之后创建的应用，已有应用的地址不变。
- **以后扩展**：多环境时每个环境配置自己的应用根域名，同一应用在各环境使用相同的 `<应用名>-<后缀>`；多部署时其他部署的地址另行设计。
- **自定义域名**：仍是应用配置，与默认地址分开验证 DNS 与证书。

控制台的会话与 CSRF Cookie 使用 `__Host-` 前缀（`Secure`、`Path=/`、不带 `Domain`），Origin 检查严格等于 `PUBLIC_BASE_URL`。

## 4. 集群、环境与部署

平台管理的是 k3s 集群，不管理单台机器。集群内机器的加入和移除由 k3s 负责，平台只读展示。

| 概念 | 含义 | 第一版 |
|---|---|---|
| 集群 | 一个 k3s 集群，集群内运行一个 worker（`yummy-system` 命名空间中的 Deployment），由全局管理员接入 | 每个实例一个，构建和应用都在其中 |
| 环境 | 应用运行的目标，例如测试、生产；绑定一个运行集群和应用根域名，由全局管理员定义 | 只有默认环境 `prod`（显示名称"生产"），界面不显示 |
| 部署 | 应用在一个环境里的运行单元，对应一个 K8s Deployment | 每个应用一个 `default`，界面不显示 |

第一版安装后实际存在的对象：

```text
全局管理员
├── 集群 main（k3s，构建 + 运行）
└── 环境 prod → 集群 main，应用根域名 example.net
工作空间 acme-studio（显示名称 Acme Studio）
└── 应用 gateway-api（后缀 k7m2x9）
    └── 部署 default（环境 prod）
        ├── 运行命名空间        prod-acme-studio
        ├── Deployment/Service  gateway-api-default
        └── 默认地址            gateway-api-k7m2x9.example.net
```

规则：

- **构建与环境无关**：同一个镜像可以发布到任何环境。构建 Job 在集群内只调度到通过构建检查的 amd64 机器（worker 检查后给机器打标签，Job 用节点选择器）。
- **部署记录**：`application_deployments` 替代原来的 `ApplicationPlacement`。首次部署时冻结集群与命名空间，首发失败也保留；之后环境改绑集群或修改实例策略，都不迁移已有部署。
- **归属**：运行配置版本、发布、运行观察、运行日志与指标都归属部署记录。以后多部署时哪些配置在部署之间共享，扩展时再定。
- **组织层级不变**：业务组织仍只有"工作空间 → 应用"两层；环境和部署描述应用运行在哪里，不是组织层级。
- **术语**："部署"指运行单元；一次镜像与运行配置版本的上线称为"发布"（Release）。应用页原来叫"部署"的页签改称"发布"，路由改为 `releases`。"构建并部署""部署更改"等动作说法不变。
- **扩展顺序**：多环境（可与生产共用集群）→ 一个应用多个部署（例如同一镜像的 web 与后台 worker）→ 一个环境多个集群。每一步分别设计权限（例如谁能发布到生产）、配置共享与地址规则。

## 5. K8s 名称

| 对象 | 规则 | 例子 |
|---|---|---|
| 环境标识 | 小写字母和数字，字母开头，最长 10 位；不能用 `kube`、`k3s`、`yummy`、`build` | `prod` |
| 部署名 | 小写字母和数字，字母开头，最长 10 位；第一版固定 | `default` |
| 运行命名空间 | `<环境>-<空间名>` | `prod-acme-studio` |
| 构建命名空间 | `build-<空间名>` | `build-acme-studio` |
| worker 命名空间 | 固定 | `yummy-system` |
| Deployment / Service | `<应用名>-<部署名>` | `gateway-api-default` |

- **不会撞名**：环境标识与部署名不含中划线，`<环境>-<空间名>` 和 `<应用名>-<部署名>` 都只有一种拆法，最长 41 位。
- **不直接用空间名作命名空间**：`default`、`kube-system` 也符合命名规范；同一空间还需要构建命名空间；以后共用一个集群的多个环境需要分开。加了环境前缀和保留词后，任何空间名都拼不出系统命名空间。
- **归属核对**：按对象作用域设置平台归属标签。运行 Namespace 与空间隔离 NetworkPolicy 归属 workspace_id 和环境；构建 Namespace 归属 workspace_id，不绑定某个应用。应用运行资源另带 application_id、deployment；构建 Job 另带 application_id 与构建/attempt 标识，不绑定运行部署。`yummy-system` 及 worker 属于实例基础设施，不带业务应用归属。worker 遇到同名、但没有平台标签或对应作用域的归属不符的对象一律报错，不接管、不覆盖；同空间的多个应用复用空间级对象。release/generation 是可更新的版本标记，不作为固定归属键。

## 6. 网络隔离

只隔离工作空间。worker 创建运行命名空间时下发：

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: yummy-workspace-isolation
  namespace: prod-acme-studio
spec:
  podSelector: {}              # 本命名空间的全部 Pod
  policyTypes: [Ingress]       # 只限制入站
  ingress:
    - from:
        - podSelector: {}      # 同一命名空间，即同一空间、同一环境
        - namespaceSelector:   # Traefik，公网请求的入口
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              app.kubernetes.io/name: traefik
```

- **必须放行 Traefik**：否则所有 HTTP 应用都无法访问。这是"只允许本空间访问"能正常工作的前提，不算额外规则。
- **健康检查**：kubelet 的探针来自 Pod 所在机器，K8s 规定这类流量始终放行；worker 在集群内检查健康时经过 Traefik，不直连 Pod。
- **环境之间**：命名空间按"环境-空间"划分，以后同一空间的测试与生产也互不相通。
- **构建命名空间**：不加策略；构建 Pod 不接受入站连接，运行命名空间也不接受来自构建命名空间的连接。
- **暂不添加**：出站限制、Pod 安全等级、资源配额都等有需要再加，避免出问题时难以排查。出站不受限，因此"PostgreSQL、Redis 只监听本机或内网，并由主机防火墙限制"这条安装要求保留。
- **M0 验证**：安装 k3s 时没有 `--disable-network-policy`（k3s 内置的网络策略控制器默认启用）；跨空间访问被拒绝；Traefik 转发与健康探针正常；Traefik Pod 的实际标签与策略一致。

## 7. 最小 CLI

第一版 CLI 是开发者电脑上的独立二进制，暂定名 `yummy`，不与服务端程序 `platform` 重名。它按需运行，不是常驻服务。

| 命令 | 作用 |
|---|---|
| `yummy login --server <控制台地址>` | 粘贴在网页"访问令牌"页创建的 PAT，校验后保存到系统凭证库；无人值守时用环境变量 `YUMMY_TOKEN` 提供 |
| `yummy use <空间名>/<应用名>` | 把当前目录关联到应用；关联保存在用户配置目录，不写进项目源码，也不扩大令牌权限 |
| `yummy deploy [目录]` | 按 `.dockerignore` 和平台排除规则打包，上传、构建、部署并等待结果；`--dry-run` 只预览文件清单和目标，`--build-only` 只构建，`--no-wait` 拿到构建 ID 后退出 |
| `yummy status` | 当前运行版本、最近构建与发布结果、访问地址 |
| `yummy logs` | 运行日志；`--build <构建 ID>` 看构建日志，`--follow` 持续跟随 |
| `yummy mcp --allow-root <目录>` | 本地 stdio MCP，供 Agent 调用；复用 CLI 凭证，只读取允许的目录 |

- 所有命令支持 `--output json`，返回稳定 ID、状态、错误码和是否可重试。
- 通过 GitHub Releases 提供 macOS（arm64、amd64）和 Linux（amd64、arm64）二进制及校验和。
- 第一版不做：浏览器 OAuth 登录、远程 HTTP MCP、自动更新、交互式向导。回滚、指标与发布历史在网页和本地 stdio MCP 中提供，CLI 以后按需补充。
- OAuth 授权服务器与远程 HTTP MCP 按 [OAuth 方案](2026-10-23-oauth-mcp-auth-design.md) 放到第一版之后，届时仍集成在控制面内。

## 8. 验收

- **名称**：1 位、30 位、31 位、含大写、数字开头、中划线结尾、含 `--`、重名等用例，在页面、API、CLI 中结果一致；空间显示名称可改，空间名和应用名不可改。
- **地址**：新应用得到 `<应用名>-<6 位后缀>`，重启或修改应用根域名后已有地址不变；后缀冲突时重新生成。应用根域名与控制台同属一个可注册域名时拒绝保存。
- **K8s**：命名空间与 Deployment 名称符合第 5 节；同空间两个应用可复用 Namespace 与隔离策略，各自的应用资源互不接管；遇到没有平台标签或对应作用域归属不符的同名对象时报错，不覆盖；同一部署发布新版本时可更新 release/generation。
- **网络**：第 6 节的 M0 验证项。
- **CLI**：在 Apple Silicon 上 `yummy deploy` 完成上传、AMD64 构建与部署；`--dry-run` 不上传；Agent 通过 `yummy mcp` 完成一次部署并读取日志。

## 9. 原型同步

[交互原型](../../prototypes/v1-build-console/index.html)已同步：应用与空间创建的命名校验、空间显示名称、默认地址后缀、集群页（只读机器列表）、发布页签、最小 CLI 命令，以及标为"后续版本"的远程 MCP 与 OAuth 页面。检查记录见 [QA](../../prototypes/v1-build-console/QA.md)。
