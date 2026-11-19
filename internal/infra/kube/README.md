# worker Kubernetes 适配器

本包只承担 worker 内的 client-go 执行适配，不读取本机 kubeconfig，不自动连接环境默认集群，不执行 SSH/kubectl，也不修改控制面数据库。只显式注入 `kubernetes.Interface`，或通过 `NewFromRESTConfig` 接收已授权的受限 TLS 凭据；生产没有默认 fake。测试 fake 只验证请求、对象及恢复逻辑，不具备 admission、调度器或真实控制器的行为。

## 控制面接线契约

- `New(client, Config)` 固定节点名/UID、分离的 build/runtime namespace 与安装资格。`InstalledCapabilities` 的 `QualificationID` 必须指向安装者实际验收记录，build/runtime/ingress/metrics 分开控制；API 可达不会自动开启能力。runner 镜像必须是非 latest 的 `image@sha256`。
- `BuildTask` 冻结 attempt、`JobName`、必填且相同的 `StableJobName`、空间/应用、节点 UID、平台、源码 grant、输出引用、构建参数/密钥版本。父级自己的确定命名算法是事实来源；本包 `JobName` helper 仅提供可选纯函数，不能替换控制面已持久名称。
- `JobUID/Acknowledged` 必须从持久 attempt 恢复，不能重启时丢弃。`Run` 先 get、核对标签/UID/快照/安全 spec，只有 NotFound 且从未确认执行过才 create；AlreadyExists 同名核对，不换名、不补跑已丢失 Job。创建未知交给下一轮同名 get。
- 源码 URI 必须 HTTPS 且无 userinfo/query/fragment；授权值、registry dockerconfig、BuildKit secret 只进入有界 immutable 临时 Secret。prepare 只挂载下载授权，build 只挂载 registry 和 build secrets；源码授权不进入构建容器。固定 runner 必须安全校验归档并使用 BuildKit secret mount，绝不用 ARG/ENV 传秘密。
- `BuildEvidence.Digest` 来自成功 build 容器终止消息中经过 runner 压缩的小型 BuildKit metadata，不信日志。`Complete` 是 Kubernetes Job 事实，不是业务构建成功；父级必须再独立验证 registry manifest/config/repository/platform。
- `Cancel` 删除 Job 和所有核对过 owner UID 的 Pod，重新 get/list 查证后才 `Stopped=true`。API 未知、删除仍在进行都不能确认；取消不依赖已经到期的源码授权、registry 凭据或节点 Ready。先核对本节点 UID，不允许同名节点替换后误删。
- `Runtime(GenerationGuard)` 的 guard 必须每次查询控制面持久期望 operation/generation/release；适配器每个写 API 前调用，不缓存永久真相。控制面仍需串行化应用及在途调用，数据库与 Kubernetes 没有跨系统原子事务。
- `Release` 固定 image@sha256/configRevision/Placement；`Apply` 直接使用 `runtimeconfig.Materialized`（仅内部，绝不返回用户 DTO），生成 Deployment/Service/Ingress/ConfigMap/Secret/PVC。配置数据以 release 命名不可变；PVC 以应用/卷命名只创建或核对，不删除、不缩容、不暗中扩容。回滚复用旧 release 配置，不回滚持久数据。
- HTTP `Route` 只能由控制面登记域名与证书绑定提供；后台进程不创建入口。当前支持管理员预置 TLS Secret（Traefik/ACME 证书编排仍由控制面/安装侧完成），不接任意 ingress 注解。
- `Stop` 仅将 replicas 写为 0，不等于已停；`Start/Restart` 使用 operation marker 幂等，不因重投再次触发模板变更。无卷 surge=1/unavailable=0；有卷 Recreate，首期一个期望实例。
- `Observe` 核对 Deployment observedGeneration、模板/实际镜像 digest、受管 ReplicaSet/Pod UID、唯一 Ready 实例、旧 Pod 退出、Service EndpointSlice；返回证据。HTTP 的 `RequiresIngressProbe` 必须再由控制面验证已登记入口/TLS/健康路径，不能凭 Running 或 KubernetesReady 宣告真实 URL 成功。
- `FollowLogs` 返回 `io.ReadCloser`，调用者必须 Close，按 PodUID/container/restart 去重。stream 打开后再次核对 PodUID/restart 防止同名替换，不能保证 Kubernetes 日志轮转不丢数据。日志落盘/脱敏/ack 在 worker 其他模块。
- `Metrics` 显式接收本节点 Kubelet HTTPS `/metrics/resource`、CA、受限 nodes/metrics 凭据，禁止 nodes/proxy/不安全 TLS/重定向。Prometheus `expfmt` 解析，按受管 PodUID/app/release 输出 samples；CPU 使用相同 startup 下 counter 差值/真实 interval，首样本、reset、缺测为 nil；memory 为 working set。不新增 TSDB、不写控制面库。

## 安全与运维限制

构建 Pod backoffLimit=0、restart Never、有界 deadline、无短 TTL；rootless UID 1000、drop ALL、禁止 privileged/host namespaces/hostPath/socket/SA token，资源 limits 与 emptyDir 有界。不兼容就保持 not-ready，seccomp/AppArmor/no-process-sandbox 放宽必须有明确安装资格开关，绝不自动降级。rootless BuildKit 不是对任意陌生租户的强隔离承诺。

临时 Secret 只在确认停止后清理；正常完成后的 Job/Secret 应在父级确认日志和结果落盘后通过显式清理/Cancel 收敛，不由短 TTL 丢弃恢复证据。Kubernetes Secret 需要安装侧静态加密、最小 RBAC 及备份管理；临时 Kube Secret 不让长期 registry 机器人密码变成即时可吊销令牌。

部署示例只用于人工审查，程序不自动 apply。RBAC namespace 最小化，节点权限 resourceNames 固定本节点，无 cluster-admin/exec/attach/nodes-proxy/PVC delete。NetworkPolicy 是否实际执行取决于 CNI，必须专项验证；示例保留 IP 不能直接用于业务，也不承诺 FQDN 白名单。

## 验证状态

当前仅 ARM Mac 上 Go fake/spec/HTTP TLS 单元测试，未读取用户 kubeconfig，未连接或修改用户集群。原生 Linux/AMD64、固定 k3s/内核/LSM/BuildKit runner digest、真正 rootless 构建推拉、TLS/Traefik、PVC 保留、Kubelet Webhook RBAC 与指标精度、NetworkPolicy 的 CNI 执行全部等待用户专用服务器资格验收；不能把测试传入的资格值用于生产 ready。
