import type { MessageInitShape } from "@bufbuild/protobuf";
import { useRef, useState } from "react";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { z } from "zod";
import { DeploymentService, type ExpectedRuntime, type RuntimeStatus, type PreviewDeploymentResponse, CreateDeploymentRequestSchema, type Deployment } from "../gen/platform/v1/deployment_pb";
import { InventoryService } from "../gen/platform/v1/inventory_pb";
import { RuntimeConfigService, type RuntimeConfigChange } from "../gen/platform/v1/runtime_config_pb";
import { invalidateMethods, useRPC } from "../lib/queries";
import { can, formatTime, platform } from "../lib/platform";
import { useApplication, useCursor } from "./workspaces";
import { Button } from "../components/ui/button";
import { Badge } from "../components/ui/badge";
import { ConfirmAction, DataForm, DataTable, Empty, Notice, Pager, QueryState, required } from "../components/console-kit";
import { RuntimeObservation } from "../components/runtime-observation";
import { RuntimeChanges } from "../components/runtime-changes";

export const deploymentState = (state: string) => ({ QUEUED: "排队中", APPLYING: "应用变更中", WAITING_READY: "等待实例就绪", VERIFYING_INGRESS: "验证访问入口", RECONCILING: "核对结果中", SUCCEEDED: "操作成功", FAILED: "操作失败", SUPERSEDED: "已被新操作替代" }[state] ?? state);
export const deploySubNav = (app: ReturnType<typeof useApplication>, workspaceSlug: string) => <nav className="tabs-nav" aria-label="部署导航"><Link to="/w/$workspaceSlug/apps/$applicationSlug/deployments" params={{ workspaceSlug, applicationSlug: app.name }} activeProps={{ "aria-current": "page" }}>部署记录</Link>{can(app, "config:read") && <Link to="/w/$workspaceSlug/apps/$applicationSlug/deployments/runtime-config" params={{ workspaceSlug, applicationSlug: app.name }} activeProps={{ "aria-current": "page" }}>运行配置</Link>}</nav>;

export function DeploymentsPage() {
  const app = useApplication(); const { workspaceSlug } = useParams({ strict: false }) as { workspaceSlug: string }; const { cursor, onPage } = useCursor();
  const query = useRPC(DeploymentService.method.listDeployments, { workspaceId: app.workspaceId, applicationId: app.id, limit: 24, cursor }, { enabled: can(app, "deployment:read"), refetchInterval: 5000 });
  const status = useRPC(DeploymentService.method.getRuntimeStatus, { workspaceId: app.workspaceId, applicationId: app.id }, { enabled: can(app, "runtime:read"), refetchInterval: 5000 });
  if (!can(app, "deployment:read")) return <Notice>没有读取发布记录的权限。</Notice>;
  return <div className="stack">{deploySubNav(app, workspaceSlug)}<div><h2 className="text-lg font-semibold">部署与运行</h2><p className="mt-1 text-sm text-muted-foreground">发布操作结果、当前服务版本与运行观察互相独立；旧版本可在新发布失败后继续服务。</p></div>{can(app, "runtime:read") && <QueryState query={status}>{status.data?.status && <RuntimeSummary status={status.data.status} />}</QueryState>}{can(app, "runtime:read") && <RuntimeObservation status={status.data?.status} statusPending={status.isPending} />}{can(app, "deployment:write") && <CreateDeploymentForm />}<QueryState query={query}>{query.data?.deployments.length ? <><DataTable label="应用发布记录" rows={query.data.deployments} columns={[
    { id: "id", title: "发布 / 操作", cell: (deployment) => <div><Link className="text-link text-xs" to="/w/$workspaceSlug/apps/$applicationSlug/deployments/$operationId" params={{ workspaceSlug: workspaceSlug, applicationSlug: app.name, operationId: deployment.operation?.id ?? "" }}><code>{deployment.operation?.id}</code></Link><code className="mt-1 block text-xs text-muted-foreground">{deployment.release?.id}</code></div> },
    { id: "state", title: "发布结果", cell: (deployment) => <div><Badge variant={deployment.operation?.state === "FAILED" ? "destructive" : "outline"}>{deploymentState(deployment.operation?.state ?? "未知")}</Badge><p className="mt-1 text-xs text-muted-foreground">{deployment.operation?.action} · generation {deployment.operation?.generation}</p></div> },
    { id: "config", title: "冻结版本", cell: (deployment) => <div className="text-xs"><p>运行配置 v{deployment.release?.runtimeConfigRevision}</p><code>{deployment.release?.artifact?.digest || "尚无镜像摘要"}</code></div> },
    { id: "time", title: "创建时间", cell: (deployment) => <span className="whitespace-nowrap text-xs">{formatTime(deployment.operation?.createdAt ?? "")}</span> },
  ]} /><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="尚无发布操作" description="先完成真实构建与产物校验，再绑定已确认的运行配置创建部署。" />}</QueryState></div>;
}
function RuntimeSummary({ status }: { status: RuntimeStatus }) {
  const app = useApplication(); const client = useQueryClient();
  const [expected, setExpected] = useState<Omit<ExpectedRuntime, "$typeName">>(); const [checked, setChecked] = useState(false); const key = useRef(crypto.randomUUID());
  async function inspect() {
    const [fresh, config] = await Promise.all([platform.deployments.getRuntimeStatus({ workspaceId: app.workspaceId, applicationId: app.id }), platform.runtime.getRuntimeConfig({ workspaceId: app.workspaceId, applicationId: app.id })]);
    if (!fresh.status || !config.config) throw new Error("没有返回完整的运行状态或配置版本，不能继续操作。");
    setExpected({ generation: fresh.status.generation, servingReleaseId: fresh.status.servingReleaseId, configRevision: config.config.revision }); key.current = crypto.randomUUID(); setChecked(true);
  }
  return <section className="section space-y-5"><h3 className="font-semibold">当前运行观察</h3><dl className="metadata"><div><dt>当前服务 Release</dt><dd><code>{status.servingReleaseId || "尚无已确认服务版本"}</code></dd></div><div><dt>期望 / 观察状态</dt><dd>{status.desiredState || "未设置"} / {status.observedState || "未知"}</dd></div><div><dt>意图 / 观察 generation</dt><dd>{status.generation} / {status.observedGeneration}</dd></div><div><dt>最后观察</dt><dd>{formatTime(status.lastObservedAt)}</dd></div></dl>{status.observedGeneration !== status.generation && <Notice tone="warning">运行观察尚未追上最新意图，不能据此宣告当前操作已生效。</Notice>}<Notice>观察状态只展示后端证据，不将构建成功、发布受理或已保存配置解释为健康；无样本不补零。</Notice>{(can(app, "runtime:write") || can(app, "deployment:write")) && <div className="space-y-3"><DataForm fields={[]} submitLabel="检查运行状态以准备操作" onSubmit={inspect} />{checked && expected && <div className="space-y-3"><p className="text-xs text-muted-foreground">已检查 generation {expected.generation}，当前服务 {expected.servingReleaseId || "无"}，最新配置 v{expected.configRevision}。变更后需重新检查。</p><div className="flex flex-wrap gap-2">{[{ value: "START", label: "启动", capability: "runtime:write" }, { value: "STOP", label: "停止", capability: "runtime:write" }, { value: "RESTART", label: "重启", capability: "deployment:write" }].filter((action) => can(app, action.capability)).map((action) => <ConfirmAction key={action.value} label={action.label} destructive={action.value !== "START"} description={`${action.label}当前应用。绑定已检查的运行状态，服务端会再次检查并返回异步操作，不提前宣告已${action.label}。`} onConfirm={async () => { await platform.deployments.changeRuntime({ workspaceId: app.workspaceId, applicationId: app.id, action: action.value, expected, idempotencyKey: `${key.current}-${action.value}` }); setChecked(false); await invalidateMethods(client, ["GetRuntimeStatus", "ListDeployments"]); }} />)}</div></div>}</div>}</section>;
}
function CreateDeploymentForm() {
  const app = useApplication(); const client = useQueryClient(); const search = useSearch({ strict: false }) as { artifactId?: string };
  const runtime = useRPC(RuntimeConfigService.method.getRuntimeConfig, { workspaceId: app.workspaceId, applicationId: app.id });
  const policy = useRPC(InventoryService.method.resolveExecution, { workspaceId: app.workspaceId, applicationId: app.id, expectedPolicyRevision: 0n, runtime: true });
  const [request, setRequest] = useState<MessageInitShape<typeof CreateDeploymentRequestSchema>>();
  const [preflight, setPreflight] = useState<PreviewDeploymentResponse>();
  const inputRevision = useRef(0);
  const [comparison, setComparison] = useState<{ revision: bigint; changes: RuntimeConfigChange[] }>();
  const [result, setResult] = useState<Deployment>(); const idempotency = useRef({ fingerprint: "", key: "" });
  return <section className="section space-y-5"><div><h3 className="font-semibold">部署新版本</h3><p className="mt-1 text-xs text-muted-foreground">每次部署把应用的一个构建产物跑起来。部署使用最新已保存的运行配置（只有已确认的配置才能部署）；保存配置与部署生效是分开的两步。</p></div><QueryState query={policy}>{policy.data?.summary && <Notice tone={policy.data.summary.ready ? "neutral" : "warning"}>策略 v{policy.data.summary.policyRevision} · {policy.data.summary.ready ? "当前平台资格通过" : policy.data.summary.reason || "未就绪"} · {policy.data.summary.platform}<span className="block break-all">默认地址：{policy.data.summary.defaultDomain || "尚未配置"}</span></Notice>}</QueryState><DataForm key={`${runtime.data?.config?.revision ?? 0n}:${search.artifactId ?? ""}`} onValuesChange={() => { inputRevision.current++; setRequest(undefined); }} fields={[{ name: "artifactId", label: "镜像版本（产物 ID）", value: search.artifactId, schema: required, hint: "填入构建页里某次成功构建的产物 ID（Artifact ID），例如从构建记录或构建完成页复制。只能部署本应用构建并通过平台验证的镜像，不接受外部镜像地址。" }, { name: "runtimeRevision", label: "运行配置版本（只读）", readOnly: true, value: runtime.data?.config?.revision.toString() ?? "", schema: z.string().regex(/^\d+$/).refine((value) => BigInt(value) > 0n, "请先保存并确认运行配置") }]} submitLabel="检查部署更改" onSubmit={async (value) => {
    setRequest(undefined);
    const inspectedInput = inputRevision.current;
    const [status, config, execution] = await Promise.all([platform.deployments.getRuntimeStatus({ workspaceId: app.workspaceId, applicationId: app.id }), platform.runtime.getRuntimeConfig({ workspaceId: app.workspaceId, applicationId: app.id }), platform.inventory.resolveExecution({ workspaceId: app.workspaceId, applicationId: app.id, expectedPolicyRevision: 0n, runtime: true })]);
    if (!status.status || !config.config || !execution.summary) throw new Error("缺少最新运行状态、配置或策略，无法安全提交。");
    if (!config.config.confirmed || BigInt(value.runtimeRevision) !== config.config.revision) throw new Error("请选择当前最新且已确认的运行配置；历史版本恢复请通过回滚操作。");
    if (!execution.summary.ready) throw new Error(execution.summary.reason || "平台资源尚未就绪");
    const payload = { workspaceId: app.workspaceId, applicationId: app.id, artifactId: value.artifactId, runtimeConfigRevision: BigInt(value.runtimeRevision), expectedPolicyRevision: execution.summary.policyRevision, expected: { generation: status.status.generation, servingReleaseId: status.status.servingReleaseId, configRevision: config.config.revision } };
    const fingerprint = JSON.stringify(payload, (_, value) => typeof value === "bigint" ? value.toString() : value);
    if (idempotency.current.fingerprint !== fingerprint) idempotency.current = { fingerprint, key: crypto.randomUUID() };
    const baseRevision = status.status.servingConfigRevision;
    const [checked, compared] = await Promise.all([
      platform.deployments.previewDeployment(payload),
      baseRevision > 0n
        ? platform.runtime.compareRuntimeConfigs({ workspaceId: app.workspaceId, applicationId: app.id, baseRevision, targetRevision: config.config.revision })
        : undefined,
    ]);
    if (inspectedInput !== inputRevision.current) return;
    setPreflight(checked);
    setComparison(compared && { revision: baseRevision, changes: compared.changes });
    setRequest({ ...payload, idempotencyKey: idempotency.current.key }); setResult(undefined);
  }} />{request && <div className="space-y-4 border-t pt-5"><Notice>将部署产物 {request.artifactId}，绑定运行配置 v{request.runtimeConfigRevision}、策略 v{request.expectedPolicyRevision}、generation {request.expected?.generation}。配置、变量、文件和卷一起生效，当前实例直到操作成功前不提前标为更新。</Notice>{preflight && <DeploymentPreflight preview={preflight} />}{comparison ? <div className="space-y-2"><h4 className="font-medium">运行配置 v{comparison.revision} → v{request.runtimeConfigRevision}</h4><RuntimeChanges changes={comparison.changes} /></div>
    : <p className="text-sm text-muted-foreground">当前没有生效版本可对比。</p>}<ConfirmAction label="确认部署此版本" description="服务端将重新校验所有冻结版本，创建发布和运行操作。受理成功不表示实例已就绪或 HTTPS 已可用。" onConfirm={async () => { const response = await platform.deployments.createDeployment(request); if (!response.deployment) throw new Error("未返回发布操作，请刷新记录确认后再重试。"); setResult(response.deployment); setRequest(undefined); await invalidateMethods(client, ["GetRuntimeStatus", "ListDeployments"]); }} /></div>}{result && <Notice tone="success">发布操作已受理：{result.operation?.id}。请根据实际操作状态和运行观察确认是否生效。</Notice>}</section>;
}
export function DeploymentDetailPage() {
  const app = useApplication(); const { operationId } = useParams({ strict: false }) as { operationId: string }; const client = useQueryClient();
  const query = useRPC(DeploymentService.method.getDeployment, { workspaceId: app.workspaceId, applicationId: app.id, operationId }, { enabled: can(app, "deployment:read"), refetchInterval: 4000 });
  const [rollback, setRollback] = useState<{ expected: Omit<ExpectedRuntime, "$typeName">; policyRevision: bigint; key: string; baseRevision: bigint; preflight: PreviewDeploymentResponse; changes?: RuntimeConfigChange[] }>();
  const deployment = query.data?.deployment;
  if (!can(app, "deployment:read")) return <Notice>没有读取此发布的权限。</Notice>;
  return <QueryState query={query} empty={!deployment}>{deployment && <div className="stack"><div><h2 className="text-lg font-semibold">发布操作</h2><code className="text-sm">{operationId}</code></div><section className="section"><dl className="metadata"><div><dt>动作 / 结果</dt><dd>{deployment.operation?.action} / {deploymentState(deployment.operation?.state ?? "未知")}</dd></div><div><dt>Release / generation</dt><dd><code>{deployment.release?.id}</code> / {deployment.operation?.generation}</dd></div><div><dt>运行配置 / 平台策略</dt><dd>v{deployment.release?.runtimeConfigRevision} / v{deployment.release?.placement?.policyRevision}</dd></div><div><dt>运行节点 / 架构</dt><dd><code>{deployment.release?.placement?.nodeId}</code> / {deployment.release?.artifact?.platform}</dd></div><div className="sm:col-span-2"><dt>镜像 Digest</dt><dd><code>{deployment.release?.artifact?.repository}@{deployment.release?.artifact?.digest}</code></dd></div><div><dt>域名</dt><dd>{deployment.release?.domains.join("、") || "尚无域名"}</dd></div><div><dt>操作错误</dt><dd>{deployment.operation?.errorCode || "尚无错误代码"}</dd></div></dl></section><Notice>发布记录是冻结快照，操作成功与当前健康需要结合最新运行观察判断。访问入口还需后端真实证书与探测证据。</Notice>{can(app, "deployment:write") && deployment.release && <section className="section space-y-4"><h3 className="font-semibold">回滚到此 Release</h3><p className="text-sm text-muted-foreground">恢复此版本的镜像和运行配置；保留最新草稿，不回退数据库或持久卷内容。</p><DataForm fields={[]} submitLabel="检查当前状态以准备回滚" onSubmit={async () => { setRollback(undefined); const [status, config, policy] = await Promise.all([platform.deployments.getRuntimeStatus({ workspaceId: app.workspaceId, applicationId: app.id }), platform.runtime.getRuntimeConfig({ workspaceId: app.workspaceId, applicationId: app.id }), platform.inventory.resolveExecution({ workspaceId: app.workspaceId, applicationId: app.id, expectedPolicyRevision: 0n, runtime: true })]); if (!status.status || !config.config || !policy.summary) throw new Error("没有返回完整运行状态、配置或平台策略。"); const baseRevision = status.status.servingConfigRevision;
    const targetRevision = deployment.release!.runtimeConfigRevision;
    const expected = { generation: status.status.generation, servingReleaseId: status.status.servingReleaseId, configRevision: config.config.revision };
    const [preflight, comparison] = await Promise.all([
      platform.deployments.previewDeployment({ workspaceId: app.workspaceId, applicationId: app.id, targetReleaseId: deployment.release!.id, expectedPolicyRevision: policy.summary.policyRevision, expected }),
      baseRevision > 0n
        ? platform.runtime.compareRuntimeConfigs({ workspaceId: app.workspaceId, applicationId: app.id, baseRevision, targetRevision })
        : undefined,
    ]);
    setRollback({ expected, policyRevision: policy.summary.policyRevision, key: crypto.randomUUID(), baseRevision, preflight, changes: comparison?.changes }); }} />{rollback && <>
    <DeploymentPreflight preview={rollback.preflight} />
    {rollback.changes && <div className="space-y-2"><h4 className="font-medium">运行配置 v{rollback.baseRevision} → v{deployment.release.runtimeConfigRevision}</h4><RuntimeChanges changes={rollback.changes} /></div>}
    <ConfirmAction label="确认回滚" destructive description={`将镜像与配置一起恢复到 ${deployment.release.id}，最新草稿 v${rollback.expected.configRevision} 保留；数据库和卷不回退。`} onConfirm={async () => { await platform.deployments.rollbackDeployment({ workspaceId: app.workspaceId, applicationId: app.id, targetReleaseId: deployment.release!.id, expectedPolicyRevision: rollback.policyRevision, expected: rollback.expected, idempotencyKey: rollback.key }); setRollback(undefined); await invalidateMethods(client, ["GetRuntimeStatus", "ListDeployments"]); }} /></>}</section>}</div>}</QueryState>;
}


function DeploymentPreflight({ preview }: { preview: PreviewDeploymentResponse }) {
  return <section aria-label="发布预检" className="space-y-3">
    <dl className="metadata">
      <div><dt>已验证镜像摘要</dt><dd><code>{preview.artifactDigest}</code></dd></div>
      <div><dt>目标架构</dt><dd>{preview.platform}</dd></div>
      {!!preview.domains.length && <div><dt>发布域名</dt><dd>{preview.domains.join("、")}</dd></div>}
    </dl>
    {preview.tlsStatus === "PENDING" && <Notice tone="warning">入口证书绑定待处理。域名和 HTTPS 连通性将在发布后检查。</Notice>}
    {preview.tlsStatus === "BOUND" && <Notice>已登记入口证书绑定。域名和 HTTPS 连通性将在发布后检查。</Notice>}
  </section>;
}
