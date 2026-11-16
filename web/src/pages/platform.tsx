import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { z } from "zod";
import { IdentityService } from "../gen/platform/v1/identity_pb";
import { InventoryService } from "../gen/platform/v1/inventory_pb";
import { OAuthAdminService } from "../gen/platform/v1/oauth_admin_pb";
import { WorkspaceService } from "../gen/platform/v1/resources_pb";
import { Button } from "../components/ui/button";
import { Badge } from "../components/ui/badge";
import { ConfirmAction, DataForm, DataTable, Empty, Heading, Notice, Pager, QueryState, SecretOnce, required, resourceName } from "../components/console-kit";
import { formatTime, platform, roleLabel, tokenScopes } from "../lib/platform";
import { invalidateMethods, useRPC, useUser } from "../lib/queries";
import { useCursor } from "./workspaces";
import { AllocationsPage, NodesPage, PolicyPage, RegistriesPage } from "./inventory";

export function PlatformPage({ users = false, tab }: { users?: boolean; tab?: "nodes" | "registries" | "policy" | "allocations" | "oauth-clients" }) {
  const user = useUser();
  if (!user.instanceAdmin) return <div className="page"><Notice tone="error">此页面仅供实例管理员使用。</Notice></div>;
  return <div className="page"><Heading title="全局管理" description="实例级管理范围。工作空间权限不授予基础设施或其他用户个人凭证的管理权。" /><nav className="tabs-nav" aria-label="全局管理导航"><Link to="/platform/workspaces" aria-current={!users && !tab ? "page" : undefined}>工作空间</Link><Link to="/platform/users" aria-current={users ? "page" : undefined}>用户与安全</Link>{([{ path: "nodes", label: "执行节点" }, { path: "registries", label: "输出仓库" }, { path: "policy", label: "平台策略" }, { path: "allocations", label: "资源分配" }, { path: "oauth-clients", label: "OAuth 客户端" }] as const).map((item) => <Link key={item.path} to={`/platform/${item.path}`} aria-current={tab === item.path ? "page" : undefined}>{item.label}</Link>)}</nav>{users ? <Users /> : tab === "nodes" ? <NodesPage /> : tab === "registries" ? <RegistriesPage /> : tab === "policy" ? <PolicyPage /> : tab === "allocations" ? <AllocationsPage /> : tab === "oauth-clients" ? <OAuthClientsPage /> : <Workspaces />}</div>;
}
export const PlatformWorkspacesRoute = () => <PlatformPage />;
export const PlatformUsersRoute = () => <PlatformPage users />;
export const PlatformNodesRoute = () => <PlatformPage tab="nodes" />;
export const PlatformRegistriesRoute = () => <PlatformPage tab="registries" />;
export const PlatformPolicyRoute = () => <PlatformPage tab="policy" />;
export const PlatformAllocationsRoute = () => <PlatformPage tab="allocations" />;
export const PlatformOAuthClientsRoute = () => <PlatformPage tab="oauth-clients" />;

function Workspaces() {
  const { cursor, onPage } = useCursor(); const [creating, setCreating] = useState(false); const client = useQueryClient();
  const query = useRPC(WorkspaceService.method.listWorkspaces, { limit: 24, cursor });
  const settings = useRPC(InventoryService.method.getInstanceSettings, {});
  const [savingToggle, setSavingToggle] = useState(false);
  const selfCreate = settings.data?.workspaceSelfCreate === true;
  const toggleSelfCreate = async () => {
    setSavingToggle(true);
    try {
      await platform.inventory.setWorkspaceSelfCreate({ enabled: !selfCreate });
      await invalidateMethods(client, ["GetInstanceSettings"]);
    } finally {
      setSavingToggle(false);
    }
  };
  return <div className="stack"><Notice tone={selfCreate ? "success" : "neutral"}>{selfCreate ? "所有登录用户可自助创建工作空间（SaaS 自由创建已开启）。" : "仅实例管理员可创建工作空间（SaaS 自由创建已关闭）。"}</Notice><div className="flex items-center justify-between gap-4"><h2 className="text-lg font-semibold">全部工作空间</h2><div className="flex items-center gap-3"><Button onClick={toggleSelfCreate} variant={selfCreate ? "outline" : "default"} disabled={savingToggle}>{savingToggle ? "保存中…" : selfCreate ? "关闭自助创建" : "开启自助创建"}</Button><Button onClick={() => setCreating(!creating)} variant={creating ? "outline" : "default"}><Plus />{creating ? "取消创建" : "创建空间"}</Button></div></div>{creating && <section className="section max-w-2xl"><DataForm fields={[{ name: "name", label: "工作空间名称", schema: required.max(100, "名称最多 100 字符") }, { name: "slug", label: "空间标识", schema: resourceName, hint: "小写字母、数字和连字符；在实例中唯一。" }, { name: "description", label: "空间说明", type: "textarea", schema: z.string().max(2000, "说明最多 2000 字符") }]} submitLabel="创建工作空间" onSubmit={async (value) => { await platform.workspaces.createWorkspace({ name: value.name, slug: value.slug, description: value.description }); setCreating(false); await invalidateMethods(client, ["ListWorkspaces"]); }} /></section>}<QueryState query={query}>{query.data?.workspaces.length ? <><DataTable label="全部工作空间" rows={query.data.workspaces} columns={[
    { id: "name", title: "工作空间", cell: (workspace) => <div><Link className="text-link font-medium" to="/w/$workspaceId/apps" params={{ workspaceId: workspace.id }}>{workspace.name}</Link><p className="mt-1 text-xs text-muted-foreground">{workspace.slug}</p></div> },
    { id: "description", title: "说明", cell: (workspace) => <p className="max-w-md break-words text-muted-foreground">{workspace.description || "—"}</p> },
    { id: "role", title: "你的业务权限", cell: (workspace) => roleLabel(workspace.role) },
    { id: "actions", title: "管理", cell: (workspace) => <Button asChild variant="outline" size="sm"><Link to="/w/$workspaceId/manage/settings" params={{ workspaceId: workspace.id }}>空间管理</Link></Button> },
  ]} /><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="尚无工作空间" description="创建空间后即可邀请成员，并在空间中创建应用。实例管理员自动具备各空间业务权限。" />}</QueryState></div>;
}
function Users() {
  const query = useRPC(IdentityService.method.listUsers, {}); const current = useUser(); const client = useQueryClient(); const [secret, setSecret] = useState("");
  const refresh = () => invalidateMethods(client, ["ListUsers", "ListWorkspaceMembers", "ListApplicationMembers"]);
  return <div className="stack"><Notice>停用用户或创建密码重置会立即使该用户原有会话及 PAT 失效。管理员不能查看、创建或撤销他人的个人 PAT。</Notice>{secret && <SecretOnce value={secret} title="密码重置链接已生成" onDismiss={() => setSecret("")} />}<QueryState query={query}>{query.data?.users.length ? <DataTable label="实例用户" rows={query.data.users} columns={[
    { id: "name", title: "用户", cell: (user) => <div><p className="font-medium">{user.username}{user.id === current.id && <span className="ml-2 text-xs text-muted-foreground">当前账户</span>}</p><p className="mt-1 break-all text-xs text-muted-foreground">{user.id}</p></div> },
    { id: "role", title: "实例身份", cell: (user) => user.instanceAdmin ? "实例管理员" : "普通用户" },
    { id: "status", title: "账户状态", cell: (user) => <Badge variant={user.status === "ACTIVE" ? "outline" : "secondary"}>{user.status === "ACTIVE" ? "正常" : "已停用"}</Badge> },
    { id: "actions", title: "安全操作", cell: (user) => <div className="flex flex-wrap gap-2"><ConfirmAction label={user.status === "ACTIVE" ? "停用用户" : "启用用户"} destructive={user.status === "ACTIVE"} description={user.status === "ACTIVE" ? `停用 ${user.username} 将立即终止其身份访问。最后一位实例管理员不能被停用。` : `重新启用 ${user.username}，此前失效的会话及 PAT 不会恢复。`} onConfirm={async () => { await platform.identity.setUserStatus({ userId: user.id, status: user.status === "ACTIVE" ? "DISABLED" : "ACTIVE" }); if (user.id === current.id) { await client.cancelQueries(); client.clear(); window.location.assign("/login"); } else await refresh(); }} /><ConfirmAction label="重置密码" disabled={user.status !== "ACTIVE"} description={`为 ${user.username} 生成一次性密码重置链接，原有会话及 PAT 将立即失效。请通过安全渠道发送链接。`} onConfirm={async () => { const response = await platform.identity.createPasswordReset({ userId: user.id }); if (!response.token) throw new Error("服务端未返回重置令牌"); setSecret(`${window.location.origin}/reset-password#token=${encodeURIComponent(response.token)}`); if (user.id === current.id) await client.cancelQueries(); }} /></div> },
  ]} /> : <Empty title="没有用户" description="当前没有可显示的实例用户。" />}</QueryState><p className="text-xs text-muted-foreground">用户接口当前最多返回 1000 条；协议尚未提供游标，未在前端伪造分页。</p></div>;
}

// 与服务端 validRedirect 对齐的提示性校验：https 或 http 环回地址，不含账号与片段；服务端强制为准。
const splitLines = (value: string) => value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
const splitTokens = (value: string) => value.split(/[\s,]+/).map((token) => token.trim()).filter(Boolean);
const advisoryRedirect = (raw: string) => {
  try {
    const url = new URL(raw);
    return !!url.host && !url.username && !url.password && !url.hash && (url.protocol === "https:" || (url.protocol === "http:" && (/^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(url.hostname) || url.hostname === "[::1]")));
  } catch { return false; }
};
const redirectUrisField = z.string().trim().min(1, "请至少填写一个回调地址").refine((value) => splitLines(value).length <= 20, "回调地址最多 20 个").refine((value) => splitLines(value).every(advisoryRedirect), "每个回调地址必须是 https，或 http 环回地址（127.x / [::1]），且不含账号信息或片段；以服务端校验为准");
const scopesField = z.string().refine((value) => splitTokens(value).every((scope) => scope === "offline_access" || tokenScopes.includes(scope)), "包含实例未发放的授权范围；可留空，范围合法性以服务端校验为准");
const resourcesField = z.string().trim().min(1, "请至少填写一个资源标识");
// 来源仅区分预登记与 CIMD 自动登记；未知值原样展示，不在前端编造语义。
const sourceLabel = (source: string) => source === "pre-registered" ? "预登记" : source === "cimd-unverified-publisher" ? "CIMD 自动登记（发布者未验证）" : source;
function OAuthClientsPage() {
  const { cursor, onPage } = useCursor(); const [creating, setCreating] = useState(false); const client = useQueryClient();
  const query = useRPC(OAuthAdminService.method.listOAuthClients, { limit: 24, cursor });
  return <div className="stack"><div className="flex flex-wrap items-center justify-between gap-3"><div><h2 className="text-lg font-semibold">OAuth 客户端</h2><p className="mt-1 text-sm text-muted-foreground">预登记客户端与 CIMD 自动登记的公共客户端都在此管理；视图不暴露任何客户端凭证。</p></div><Button onClick={() => setCreating(!creating)} variant={creating ? "outline" : "default"}><Plus />{creating ? "取消注册" : "注册客户端"}</Button></div><Notice>停用客户端会立即撤销该客户端的全部既有授权；重新启用不会恢复已撤销的授权。</Notice>{creating && <section className="section max-w-2xl"><h3 className="section-title">注册 OAuth 客户端</h3><p className="mb-5 text-sm text-muted-foreground">登记的是无密钥公共客户端；client_id 重复会返回冲突，不做覆盖。</p><DataForm fields={[
    { name: "clientId", label: "客户端 ID（client_id）", schema: required.max(2048, "客户端 ID 最长 2048 字符"), hint: "客户端在授权请求中提交的唯一标识，重复登记返回冲突。" },
    { name: "name", label: "客户端名称", schema: required.max(200, "名称最长 200 字符") },
    { name: "redirectUris", label: "回调地址（每行一个）", type: "textarea", schema: redirectUrisField, hint: "https 或 http 环回地址；此处为提示性校验，服务端强制为准。" },
    { name: "scopes", label: "授权范围（空格或逗号分隔，可留空）", type: "textarea", schema: scopesField, hint: "仅接受实例令牌范围及 offline_access；以服务端校验为准。" },
    { name: "resources", label: "资源（每行一个）", type: "textarea", schema: resourcesField, hint: "本实例 API 资源或应用 MCP 规范地址；以服务端校验为准。" },
  ]} submitLabel="登记客户端" onSubmit={async (value) => { await platform.oauthAdmin.registerOAuthClient({ clientId: value.clientId, name: value.name, redirectUris: splitLines(value.redirectUris), scopes: splitTokens(value.scopes), resources: splitLines(value.resources) }); setCreating(false); await invalidateMethods(client, ["ListOAuthClients"]); }} /></section>}<QueryState query={query}>{query.data?.clients.length ? <><DataTable label="OAuth 客户端" rows={query.data.clients} columns={[
    { id: "clientId", title: "client_id", cell: (row) => <code className="block max-w-xs break-all text-xs">{row.clientId}</code> },
    { id: "name", title: "名称", cell: (row) => <p className="font-medium">{row.name}</p> },
    { id: "source", title: "来源", cell: (row) => <Badge variant="outline">{sourceLabel(row.source)}</Badge> },
    { id: "redirectUris", title: "回调地址", cell: (row) => <div className="max-w-xs space-y-1">{row.redirectUris.map((uri) => <code key={uri} className="block break-all text-xs text-muted-foreground">{uri}</code>)}</div> },
    { id: "scopes", title: "授权范围", cell: (row) => <p className="max-w-xs break-words text-xs text-muted-foreground">{row.scopes.join(" · ") || "—"}</p> },
    { id: "resources", title: "资源", cell: (row) => <div className="max-w-xs space-y-1">{row.resources.map((resource) => <p key={resource} className="break-all text-xs text-muted-foreground">{resource}</p>)}</div> },
    { id: "status", title: "状态", cell: (row) => <Badge variant={row.active ? "outline" : "secondary"}>{row.active ? "已启用" : "已停用"}</Badge> },
    { id: "createdAt", title: "创建时间", cell: (row) => <span className="whitespace-nowrap text-xs text-muted-foreground">{formatTime(row.createdAt)}</span> },
    { id: "actions", title: "管理", cell: (row) => <ConfirmAction label={row.active ? "停用客户端" : "启用客户端"} destructive={row.active} description={row.active ? `停用「${row.name}」会立即撤销该客户端的全部既有授权；重新启用不会恢复这些授权。` : `重新启用「${row.name}」。此前因停用撤销的授权不会恢复，客户端需重新发起授权。`} onConfirm={async () => { await platform.oauthAdmin.setOAuthClientActive({ clientId: row.clientId, active: !row.active }); await invalidateMethods(client, ["ListOAuthClients"]); }} /> },
  ]} /><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="尚无 OAuth 客户端" description="登记预登记公共客户端，或由第三方通过 CIMD 自动登记后在此统一管理。" />}</QueryState></div>;
}
