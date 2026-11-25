import { useId, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link } from "@tanstack/react-router";
import { DeploymentService } from "../gen/platform/v1/deployment_pb";
import { InventoryService, type ExecutionNode } from "../gen/platform/v1/inventory_pb";
import { ApplicationService, type Workspace } from "../gen/platform/v1/resources_pb";
import { RuntimeConfigService } from "../gen/platform/v1/runtime_config_pb";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Label } from "../components/ui/label";
import { errorMessage } from "../lib/platform";
import { invalidateMethods, useRPC } from "../lib/queries";

const refreshMethods = ["GetExecutionPolicy", "GetNode", "GetRegistry", "ListWorkspaces",
  "GetWorkspaceAllocation", "ListApplications", "GetRuntimeStatus", "GetRuntimeConfig"];
type Check = { state: "通过" | "待处理" | "检查失败" | "检查中"; detail: string };
type QueryCheck = { isPending: boolean; error: unknown };

function notFound(error: unknown) {
  return error instanceof ConnectError && error.code === Code.NotFound;
}

function checkQuery(queries: QueryCheck[], next: () => Check): Check {
  const failure = queries.find((query) => query.error);
  if (failure) return { state: "检查失败", detail: errorMessage(failure.error) };
  if (queries.some((query) => query.isPending)) return { state: "检查中", detail: "正在读取平台状态。" };
  return next();
}

function nodeProblem(node: ExecutionNode | undefined, build: boolean) {
  if (!node) return "没有找到策略指定的执行节点。";
  if (node.status !== "ACTIVE") return `「${node.name}」未启用或正在排空。`;
  const age = Date.now() - Date.parse(node.lastSeen);
  if (!Number.isFinite(age) || age > 90_000 || age < -30_000) return `「${node.name}」的 worker 心跳缺失或已过期。`;
  if (node.os !== "linux" || node.arch !== "amd64" || !node.clusterNodeUid) return `「${node.name}」尚未通过 Linux/AMD64 节点身份检查。`;
  if (build ? !node.buildReady : !node.runtimeReady || !node.metricsReady) {
    return node.readinessReason || `「${node.name}」尚未通过${build ? "构建" : "运行与监控"}资格检查。`;
  }
  return "";
}

function Step({ title, check, children }: { title: string; check: Check; children?: ReactNode }) {
  return <li className="space-y-2 py-3">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h3 className="font-medium">{title}</h3>
      <Badge variant={check.state === "检查失败" ? "destructive" : "outline"}>{check.state}</Badge>
    </div>
    <p className="break-words text-sm text-muted-foreground">{check.detail}</p>
    {children}
  </li>;
}

export function PlatformSetup({ workspaces, workspaceQuery, onCreateWorkspace }: {
  workspaces: Workspace[];
  workspaceQuery: QueryCheck;
  onCreateWorkspace: () => void;
}) {
  const [open, setOpen] = useState(false);
  const controls = useId();
  return <section className="section space-y-3">
    <Button size="sm" variant="outline" aria-expanded={open} aria-controls={controls} onClick={() => setOpen(!open)}>接入检查</Button>
    <div id={controls} hidden={!open}>{open && <SetupChecks workspaces={workspaces} workspaceQuery={workspaceQuery} onCreateWorkspace={onCreateWorkspace} />}</div>
  </section>;
}

function SetupChecks({ workspaces, workspaceQuery, onCreateWorkspace }: {
  workspaces: Workspace[];
  workspaceQuery: QueryCheck;
  onCreateWorkspace: () => void;
}) {
  const client = useQueryClient();
  const [refreshing, setRefreshing] = useState(false);
  const [workspaceID, setWorkspaceID] = useState("");
  const workspace = workspaces.find((item) => item.id === workspaceID) ?? workspaces[0];
  const selectedWorkspaceID = workspace?.id ?? "";
  const policyQuery = useRPC(InventoryService.method.getExecutionPolicy, {});
  const policy = policyQuery.data?.policy;
  const policyStatus = { isPending: policyQuery.isPending, error: notFound(policyQuery.error) ? null : policyQuery.error };
  const buildNode = useRPC(InventoryService.method.getNode, { nodeId: policy?.buildNodeId ?? "" }, { enabled: !!policy?.buildNodeId });
  const runtimeNode = useRPC(InventoryService.method.getNode, { nodeId: policy?.runtimeNodeId ?? "" }, { enabled: !!policy?.runtimeNodeId });
  const registry = useRPC(InventoryService.method.getRegistry, { registryId: policy?.registryId ?? "" }, { enabled: !!policy?.registryId });
  const allocation = useRPC(InventoryService.method.getWorkspaceAllocation, { workspaceId: selectedWorkspaceID }, { enabled: !!workspace });
  const apps = useRPC(ApplicationService.method.listApplications, { workspaceId: selectedWorkspaceID, limit: 24 }, { enabled: !!workspace });
  const [applicationID, setApplicationID] = useState("");
  const app = apps.data?.applications.find((item) => item.id === applicationID) ?? apps.data?.applications[0];
  const runtime = useRPC(DeploymentService.method.getRuntimeStatus, { workspaceId: selectedWorkspaceID, applicationId: app?.id ?? "" }, { enabled: !!app });
  const status = runtime.data?.status;
  const servingRevision = status?.servingConfigRevision ?? 0n;
  const running = !!status?.servingReleaseId && status.observedState === "RUNNING" && status.observedGeneration === status.servingGeneration;
  const servingConfig = useRPC(RuntimeConfigService.method.getRuntimeConfig, {
    workspaceId: selectedWorkspaceID, applicationId: app?.id ?? "", revision: servingRevision,
  }, { enabled: running && servingRevision > 0n });

  const nodeCheck = checkQuery([policyStatus, ...(policy?.buildNodeId && policy?.runtimeNodeId ? [buildNode, runtimeNode] : [])], () => {
    if (!policy?.buildNodeId || !policy.runtimeNodeId) return { state: "待处理", detail: "在平台策略中选择构建和运行节点。请先登记并启动 worker。" };
    const problem = nodeProblem(buildNode.data?.node, true) || nodeProblem(runtimeNode.data?.node, false);
    return problem ? { state: "待处理", detail: problem } : { state: "通过", detail: "策略中的构建和运行节点均已有在线资格。" };
  });
  const registryCheck = checkQuery([policyStatus, ...(policy?.registryId ? [registry] : [])], () => {
    if (!policy?.registryId) return { state: "待处理", detail: "请添加输出仓库并在平台策略中选用。" };
    return registry.data?.registry?.status === "ACTIVE" && registry.data.registry.verifiedAt
      ? { state: "通过", detail: `「${registry.data.registry.name}」当前配置已验证。` }
      : { state: "待处理", detail: "输出仓库尚未验证，请先验证连接。" };
  });
  const ingressCheck = checkQuery([policyStatus, ...(policy?.runtimeNodeId ? [runtimeNode] : []),
    ...(app ? [runtime] : []), ...(running && servingRevision > 0n ? [servingConfig] : [])], () => {
    if (!policy?.baseDomain) return { state: "待处理", detail: "请设置应用根域名并配置 DNS 与 HTTPS 入口。" };
    if (!runtimeNode.data?.node?.ingressReady || nodeProblem(runtimeNode.data.node, false)) {
      return { state: "待处理", detail: `${policy.baseDomain} 已配置，运行节点尚未通过入口资格检查。` };
    }
    return running && servingConfig.data?.config?.type === "HTTP"
      ? { state: "通过", detail: `「${app?.name}」的当前发布已通过入口探测。` }
      : { state: "待处理", detail: `${policy.baseDomain} 已配置，等待 HTTP 应用的实际入口探测。` };
  });
  const workspaceCheck = checkQuery([workspaceQuery, ...(workspace ? [{ ...allocation, error: notFound(allocation.error) ? null : allocation.error }] : [])], () => {
    if (!workspace) return { state: "待处理", detail: "请创建工作空间，或返回工作空间列表首页。" };
    const allowed = allocation.data?.allocation?.allowedNodeIds;
    if (!policy || !allowed?.includes(policy.buildNodeId) || !allowed.includes(policy.runtimeNodeId)) {
      return { state: "待处理", detail: `「${workspace.name}」尚未分配当前策略所需的执行节点。` };
    }
    return { state: "通过", detail: `「${workspace.name}」已分配构建和运行节点。` };
  });
  const appCheck = checkQuery([workspaceQuery, ...(workspace ? [apps] : []), ...(app ? [runtime] : [])], () => {
    if (!app) return { state: "待处理", detail: "请在选定工作空间创建应用。" };
    return running ? { state: "通过", detail: `「${app.name}」的当前发布正在运行。` }
      : { state: "待处理", detail: `「${app.name}」当前没有可验证的运行版本。请查看部署状态。` };
  });

  return <div className="mt-3 space-y-4" aria-label="新实例接入检查">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <p className="text-sm text-muted-foreground">根据平台策略和实际运行状况检查接入进度。</p>
      <Button variant="outline" size="sm" disabled={refreshing} onClick={async () => {
        setRefreshing(true);
        try { await invalidateMethods(client, refreshMethods); } finally { setRefreshing(false); }
      }}>{refreshing ? "正在检查…" : "重新检查"}</Button>
    </div>
    <ol className="divide-y">
      <Step title="执行集群" check={nodeCheck}>
        <Link className="text-link text-sm" to="/platform/nodes">管理执行节点</Link>{" · "}
        <Link className="text-link text-sm" to="/platform/policy">选择策略节点</Link>
      </Step>
      <Step title="输出仓库" check={registryCheck}>
        <Link className="text-link text-sm" to="/platform/registries">管理仓库与验证连接</Link>
      </Step>
      <Step title="运行入口" check={ingressCheck}>
        <Link className="text-link text-sm" to="/platform/policy">配置应用根域名</Link>
      </Step>
      <Step title="工作空间" check={workspaceCheck}>
        {workspace ? <div className="flex flex-wrap items-center gap-2">
          <Label htmlFor="setup-workspace">本页工作空间</Label>
          <select id="setup-workspace" value={selectedWorkspaceID} onChange={(event) => { setWorkspaceID(event.target.value); setApplicationID(""); }}>
            {workspaces.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}
          </select>
          <Link className="text-link text-sm" to="/platform/allocations">配置资源分配</Link>
        </div> : !workspaceQuery.isPending && <Button variant="outline" size="sm" onClick={onCreateWorkspace}>开始创建工作空间</Button>}
      </Step>
      <Step title="应用" check={appCheck}>
        {workspace && (app ? <div className="flex flex-wrap items-center gap-2">
          <Label htmlFor="setup-app">应用</Label>
          <select id="setup-app" value={app.id} onChange={(event) => setApplicationID(event.target.value)}>
            {apps.data?.applications.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}
          </select>
          <Link className="text-link text-sm" to="/w/$workspaceSlug/apps/$applicationSlug/deployments" params={{ workspaceSlug: workspace.slug, applicationSlug: app.name }}>前往应用部署</Link>
          <Link className="text-link text-sm" to="/w/$workspaceSlug/apps" params={{ workspaceSlug: workspace.slug }}>全部应用</Link>
        </div> : <Link className="text-link text-sm" to="/w/$workspaceSlug/apps" params={{ workspaceSlug: workspace.slug }}>前往创建应用</Link>)}
      </Step>
    </ol>
  </div>;
}
