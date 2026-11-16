import { createContext, useContext, useState } from "react";
import { Link, Outlet, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ArrowRight, Box, Plus, Settings2 } from "lucide-react";
import { z } from "zod";
import { WorkspaceService, ApplicationService, type Workspace, type Application } from "../gen/platform/v1/resources_pb";
import { IdentityService } from "../gen/platform/v1/identity_pb";
import { Button } from "../components/ui/button";
import { Badge } from "../components/ui/badge";
import { ConfirmAction, DataForm, DataTable, Empty, Heading, Notice, Pager, QueryState, SecretOnce, required, resourceName } from "../components/console-kit";
import { can, formatTime, platform, roleLabel } from "../lib/platform";
import { invalidateMethods, useRPC } from "../lib/queries";

const WorkspaceContext = createContext<Workspace | undefined>(undefined);
const ApplicationContext = createContext<Application | undefined>(undefined);
export function useWorkspace() { const value = useContext(WorkspaceContext); if (!value) throw new Error("未加载工作空间"); return value; }
export function useApplication() { const value = useContext(ApplicationContext); if (!value) throw new Error("未加载应用"); return value; }
export function useCursor() {
  const search = useSearch({ strict: false }) as { cursor?: string };
  const navigate = useNavigate();
  return { cursor: search.cursor ?? "", onPage: (cursor: string) => void navigate({ to: ".", search: { cursor } as never }) };
}
export function WorkspaceHome() {
  const { cursor, onPage } = useCursor();
  const query = useRPC(WorkspaceService.method.listWorkspaces, { limit: 24, cursor });
  const client = useQueryClient();
  const session = useRPC(IdentityService.method.getSession, {});
  const canCreate = session.data?.workspaceSelfCreate === true || session.data?.user?.instanceAdmin === true;
  const [creating, setCreating] = useState(false);
  return <div className="page"><div className="flex items-center justify-between gap-4"><Heading title="你的工作空间" description="选择一个工作空间，开始管理你的应用。" />{canCreate && <Button onClick={() => setCreating(!creating)} variant={creating ? "outline" : "default"}><Plus />{creating ? "取消创建" : "创建工作空间"}</Button>}</div>{creating && <section className="section max-w-2xl"><DataForm fields={[{ name: "name", label: "工作空间名称", schema: required.max(100, "名称最多 100 字符") }, { name: "slug", label: "空间标识", schema: resourceName, hint: "小写字母、数字和连字符；最长 30 位，在实例中唯一。" }, { name: "description", label: "空间说明", type: "textarea", schema: z.string().max(2000, "说明最多 2000 字符") }]} submitLabel="创建工作空间" onSubmit={async (value) => { await platform.workspaces.createWorkspace({ name: value.name, slug: value.slug, description: value.description }); setCreating(false); await invalidateMethods(client, ["ListWorkspaces"]); }} /></section>}<QueryState query={query}>{query.data?.workspaces.length ? <><div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">{query.data.workspaces.map((workspace) => <Link key={workspace.id} to="/w/$workspaceId/apps" params={{ workspaceId: workspace.id }} className="group rounded-lg border bg-card p-6 transition-colors hover:border-input hover:bg-muted/40"><div className="flex items-start justify-between gap-4"><h2 className="break-all text-lg font-semibold">{workspace.name}</h2><ArrowRight className="mt-1 size-4 shrink-0 text-muted-foreground group-hover:text-foreground" /></div><p className="mt-3 line-clamp-2 min-h-10 text-sm text-muted-foreground">{workspace.description || "在此空间组织应用与团队协作。"}</p><p className="mt-5 border-t pt-4 text-xs text-muted-foreground">{roleLabel(workspace.role)} · {workspace.slug}</p></Link>)}</div><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="还没有可访问的工作空间" description={canCreate ? "创建你的第一个工作空间，开始管理应用。" : "请联系管理员获取邀请，或由管理员开启自助创建工作空间。"} />}</QueryState></div>;
}
export function WorkspaceLayout() {
  const { workspaceId } = useParams({ strict: false }) as { workspaceId: string };
  const query = useRPC(WorkspaceService.method.getWorkspace, { workspaceId });
  return <QueryState query={query} empty={!query.data?.workspace}><WorkspaceContext.Provider value={query.data?.workspace}><Outlet /></WorkspaceContext.Provider></QueryState>;
}
export function ApplicationsPage() {
  const workspace = useWorkspace(); const { cursor, onPage } = useCursor();
  const query = useRPC(ApplicationService.method.listApplications, { workspaceId: workspace.id, limit: 24, cursor });
  const [creating, setCreating] = useState(false); const [key, setKey] = useState(() => crypto.randomUUID()); const [submittedName, setSubmittedName] = useState("");
  const client = useQueryClient(); const navigate = useNavigate();
  return <div className="page"><Heading title="应用" description={`${workspace.name} 的应用与配置。`} action={<div className="flex flex-wrap gap-2">{can(workspace, "workspace:write") && <Button variant="outline" asChild><Link to="/w/$workspaceId/manage/settings" params={{ workspaceId: workspace.id }}><Settings2 />空间管理</Link></Button>}{can(workspace, "application:create") && <Button onClick={() => { setCreating(!creating); if (!creating) { setKey(crypto.randomUUID()); setSubmittedName(""); } }}><Plus />{creating ? "取消创建" : "创建应用"}</Button>}</div>} />
    {creating && <section className="section mb-8 max-w-xl"><h2 className="section-title">创建应用</h2><p className="mb-5 text-sm text-muted-foreground">归属「{workspace.name}」。只填写名称，创建后进入运行配置；不会触发构建或部署。</p><DataForm fields={[{ name: "name", label: "应用名称", schema: resourceName, hint: "例如 my-service。名称在当前空间内唯一。" }]} submitLabel="创建并配置" onSubmit={async (value) => {
      const same = submittedName === value.name; const requestKey = !submittedName || same ? key : crypto.randomUUID();
      setKey(requestKey); setSubmittedName(value.name);
      const response = await platform.applications.createApplication({ workspaceId: workspace.id, name: value.name, idempotencyKey: requestKey });
      if (!response.application) throw new Error("控制面没有返回应用，请刷新列表确认后再重试。");
      await invalidateMethods(client, ["ListApplications"]);
      await navigate({ to: "/w/$workspaceId/apps/$applicationId/settings/runtime", params: { workspaceId: workspace.id, applicationId: response.application.id } });
    }} /></section>}
    <QueryState query={query}>{query.data?.applications.length ? <><div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{query.data.applications.map((application) => <Link key={application.id} to="/w/$workspaceId/apps/$applicationId" params={{ workspaceId: workspace.id, applicationId: application.id }} className="group flex min-w-0 flex-col rounded-lg border bg-card p-6 transition-colors hover:border-input hover:bg-muted/40"><div className="mb-5 flex items-center justify-between gap-3"><Box className="size-5 text-muted-foreground" strokeWidth={1.5} /><Badge variant="outline">运行状态见详情</Badge></div><h2 className="truncate text-lg font-semibold">{application.name}</h2><p className="mt-2 line-clamp-2 min-h-10 text-sm text-muted-foreground">{application.description || "尚未填写应用说明"}</p><div className="mt-6 flex items-center justify-between gap-3 border-t pt-4 text-xs text-muted-foreground"><span>更新于 {formatTime(application.updatedAt)}</span><ArrowRight className="size-4 group-hover:text-foreground" /></div></Link>)}</div><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="从你的第一个应用开始" description={can(workspace, "application:create") ? "创建只需一个名称。运行配置保存、构建与部署是不同步骤，不会自动执行。" : "当前没有已授权的应用。请联系空间管理员获得应用访问权限。"} />}</QueryState>
  </div>;
}
export function ApplicationLayout() {
  const workspace = useWorkspace(); const { applicationId } = useParams({ strict: false }) as { applicationId: string };
  const query = useRPC(ApplicationService.method.getApplication, { workspaceId: workspace.id, applicationId });
  const application = query.data?.application;
  return <div className="page"><Link to="/w/$workspaceId/apps" params={{ workspaceId: workspace.id }} className="mb-6 inline-flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground"><ArrowLeft className="size-4" />返回应用</Link><QueryState query={query} empty={!application}>{application && <ApplicationContext.Provider value={application}><Heading title={application.name} description={application.description || "管理应用资料、配置与成员权限。"} action={<Badge variant="outline">{application.status === "ACTIVE" ? "应用有效" : application.status}</Badge>} /><nav className="tabs-nav" aria-label="应用导航"><Link to="/w/$workspaceId/apps/$applicationId" params={{ workspaceId: workspace.id, applicationId }} activeOptions={{ exact: true }} activeProps={{ "aria-current": "page" }}>概览</Link>{can(application, "deployment:read") && <Link to="/w/$workspaceId/apps/$applicationId/deployments" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>部署</Link>}{can(application, "build:read") && <Link to="/w/$workspaceId/apps/$applicationId/builds" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>构建</Link>}{can(application, "source:upload") && <Link to="/w/$workspaceId/apps/$applicationId/source" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>源码</Link>}{can(application, "config:read") && <Link to="/w/$workspaceId/apps/$applicationId/settings/runtime" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>运行配置</Link>}{can(application, "config:read") && <Link to="/w/$workspaceId/apps/$applicationId/settings/build" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>构建设置</Link>}<Link to="/w/$workspaceId/apps/$applicationId/settings/basic" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>基本信息</Link>{can(application, "member:write") && <Link to="/w/$workspaceId/apps/$applicationId/settings/access" params={{ workspaceId: workspace.id, applicationId }} activeProps={{ "aria-current": "page" }}>成员与权限</Link>}</nav><Outlet key={application.id} /></ApplicationContext.Provider>}</QueryState></div>;
}
export function ApplicationBasic() {
  const app = useApplication(); const client = useQueryClient();
  return <section className="section max-w-3xl"><h2 className="section-title">基本信息</h2><p className="mb-6 text-sm text-muted-foreground">此处只更新应用资料，不会修改运行配置或触发部署。</p><DataForm key={app.revision.toString()} disabled={!can(app, "config:write")} fields={[{ name: "name", label: "应用名称", value: app.name, schema: resourceName }, { name: "description", label: "应用说明", value: app.description, type: "textarea", schema: z.string().max(2000, "说明最多 2000 字符") }]} successMessage="应用资料已保存。" onSubmit={async (value) => { await platform.applications.updateApplication({ workspaceId: app.workspaceId, applicationId: app.id, name: value.name, description: value.description, expectedRevision: app.revision }); await invalidateMethods(client, ["GetApplication", "ListApplications", "ListWorkspaceMembers"]); }} />{!can(app, "config:write") && <p className="mt-4 text-sm text-muted-foreground">你有查看权限，但不能修改应用资料。</p>}</section>;
}
export function ApplicationAccess() {
  const app = useApplication(); const workspace = useWorkspace(); const { cursor, onPage } = useCursor(); const client = useQueryClient();
  const allowed = can(app, "member:write");
  const members = useRPC(ApplicationService.method.listApplicationMembers, { workspaceId: workspace.id, applicationId: app.id, limit: 24, cursor }, { enabled: allowed });
  const [memberCursor, setMemberCursor] = useState("");
  const candidates = useRPC(WorkspaceService.method.listWorkspaceMembers, { workspaceId: workspace.id, limit: 24, cursor: memberCursor }, { enabled: allowed });
  const refresh = () => invalidateMethods(client, ["ListApplicationMembers", "ListWorkspaceMembers", "GetApplication", "GetWorkspace", "ListApplications", "ListWorkspaces"]);
  if (!allowed) return <Notice>当前账户没有管理此应用成员的权限。</Notice>;
  return <div className="stack"><div><h2 className="text-lg font-semibold">成员与权限</h2><p className="mt-1 text-sm text-muted-foreground">授权仅作用于此应用。空间管理员与实例管理员继承访问，不能在应用中移除。</p></div><section className="section max-w-2xl"><h3 className="section-title mb-5">授予 / 更新应用权限</h3><QueryState query={candidates}><DataForm fields={[{ name: "userId", label: "空间成员", options: [{ value: "", label: "选择成员" }, ...(candidates.data?.members.filter((m) => m.role !== "admin").map((m) => ({ value: m.userId, label: `${m.username} · ${m.userId}` })) ?? [])], schema: required }, { name: "role", label: "应用角色", value: "viewer", options: applicationRoleOptions, schema: required }]} submitLabel="保存应用授权" successMessage="应用授权已保存。" onSubmit={async (value) => { await platform.applications.setApplicationMember({ workspaceId: workspace.id, applicationId: app.id, userId: value.userId, role: value.role }); await refresh(); }} /><Pager cursor={memberCursor} nextCursor={candidates.data?.nextCursor} onPage={setMemberCursor} /></QueryState></section><QueryState query={members}>{members.data?.members.length ? <><DataTable label="应用成员权限" rows={members.data.members} columns={[
    { id: "user", title: "成员", cell: (member) => <div><p className="font-medium">{member.username}</p><p className="text-xs text-muted-foreground">{member.userStatus === "ACTIVE" ? "正常" : "已停用"}</p></div> },
    { id: "role", title: "应用角色", cell: (member) => <span>{roleLabel(member.role)}{member.inherited && <Badge variant="secondary" className="ml-2">继承</Badge>}</span> },
    { id: "action", title: "操作", cell: (member) => member.inherited ? <span className="text-xs text-muted-foreground">在所属管理域维护</span> : <ConfirmAction label="移除授权" destructive description={`移除 ${member.username} 对此应用的访问，不会将其移出工作空间。`} onConfirm={async () => { await platform.applications.removeApplicationMember({ workspaceId: workspace.id, applicationId: app.id, userId: member.userId }); await refresh(); }} /> },
  ]} /><Pager cursor={cursor} nextCursor={members.data.nextCursor} onPage={onPage} /></> : <Empty title="没有显式应用成员" description="空间及实例管理员仍继承业务访问权限。可在上方授予空间成员应用权限。" />}</QueryState></div>;
}
const applicationRoleOptions = [{ value: "viewer", label: "查看者" }, { value: "developer", label: "开发者" }, { value: "maintainer", label: "维护者" }];
export function WorkspaceManagement({ tab }: { tab: "settings" | "members" | "invitations" }) {
  const workspace = useWorkspace(); const allowed = can(workspace, "workspace:write");
  if (!allowed) return <div className="page"><Notice>当前账户没有管理此工作空间的权限。</Notice></div>;
  return <div className="page"><Link to="/w/$workspaceId/apps" params={{ workspaceId: workspace.id }} className="mb-6 inline-flex items-center gap-2 text-sm text-muted-foreground"><ArrowLeft className="size-4" />返回应用</Link><Heading title="工作空间管理" description={`${workspace.name} · 仅管理本空间资料、团队身份与应用授权，不包含实例基础设施。`} /><nav className="tabs-nav" aria-label="工作空间管理导航">{([{ tab: "settings", name: "空间设置" }, { tab: "members", name: "成员与授权概览" }, { tab: "invitations", name: "邀请" }] as const).map((item) => <Link key={item.tab} to={`/w/$workspaceId/manage/${item.tab}`} params={{ workspaceId: workspace.id }} aria-current={tab === item.tab ? "page" : undefined}>{item.name}</Link>)}</nav>{tab === "settings" ? <WorkspaceSettings /> : tab === "members" ? <WorkspaceMembers /> : <WorkspaceInvitations />}</div>;
}
function WorkspaceSettings() {
  const workspace = useWorkspace(); const client = useQueryClient();
  return <section className="section max-w-3xl"><h2 className="section-title mb-6">空间资料</h2><DataForm key={workspace.revision.toString()} fields={[{ name: "name", label: "空间名称", value: workspace.name, schema: required.max(100, "名称最多 100 字符") }, { name: "slug", label: "空间标识", value: workspace.slug, schema: resourceName }, { name: "description", label: "空间说明", value: workspace.description, type: "textarea", schema: z.string().max(2000, "说明最多 2000 字符") }]} successMessage="工作空间资料已保存。" onSubmit={async (value) => { await platform.workspaces.updateWorkspace({ workspaceId: workspace.id, name: value.name, slug: value.slug, description: value.description, expectedRevision: workspace.revision }); await invalidateMethods(client, ["GetWorkspace", "ListWorkspaces"]); }} /></section>;
}
function WorkspaceMembers() {
  const workspace = useWorkspace(); const { cursor, onPage } = useCursor(); const client = useQueryClient();
  const query = useRPC(WorkspaceService.method.listWorkspaceMembers, { workspaceId: workspace.id, limit: 24, cursor });
  const refresh = () => invalidateMethods(client, ["ListApplicationMembers", "ListWorkspaceMembers", "GetApplication", "GetWorkspace", "ListApplications", "ListWorkspaces"]);
  return <div className="stack"><Notice>应用授权的主要操作入口在各应用的「成员与权限」。此处展示同一份授权概览，移出空间会收回该空间全部应用访问。</Notice><QueryState query={query}>{query.data?.members.length ? <><DataTable label="空间成员与应用授权" rows={query.data.members} columns={[
    { id: "user", title: "成员", cell: (member) => <div><p className="font-medium">{member.username}</p><p className="mt-1 break-all text-xs text-muted-foreground">{member.userId} · {member.userStatus === "ACTIVE" ? "正常" : "已停用"}</p></div> },
    { id: "role", title: "空间角色", cell: (member) => <span className="whitespace-nowrap">{roleLabel(member.role)}</span> },
    { id: "grants", title: "应用访问权", cell: (member) => member.inheritsAllApplications ? <span className="text-sm text-muted-foreground">继承所有应用</span> : <div className="max-w-xs space-y-1">{member.applications.length ? member.applications.map((grant) => <Link className="text-link block text-xs" key={grant.applicationId} to="/w/$workspaceId/apps/$applicationId/settings/access" params={{ workspaceId: workspace.id, applicationId: grant.applicationId }}>{grant.applicationName} · {roleLabel(grant.role)}</Link>) : <span className="text-xs text-muted-foreground">尚无应用授权</span>}{member.applicationsTruncated && <span className="block text-xs text-muted-foreground">仅显示部分授权；请进入各应用查看完整名单。</span>}</div> },
    { id: "actions", title: "操作", cell: (member) => <div className="flex flex-wrap gap-2"><ConfirmAction label={member.role === "admin" ? "设为普通成员" : "设为空间管理员"} description={member.role === "admin" ? `将 ${member.username} 设为普通成员，会失去空间管理与继承应用权限。最后一位管理员不能被降级。` : `${member.username} 将获得本空间管理和所有应用业务权限，不会获得实例管理权限。`} onConfirm={async () => { await platform.workspaces.changeWorkspaceMemberRole({ workspaceId: workspace.id, userId: member.userId, role: member.role === "admin" ? "member" : "admin" }); await refresh(); }} /><ConfirmAction label="移出空间" destructive description={`移出 ${member.username} 将收回本空间所有应用访问，不删除个人账户或其个人凭证。`} onConfirm={async () => { await platform.workspaces.removeWorkspaceMember({ workspaceId: workspace.id, userId: member.userId }); await refresh(); }} /></div> },
  ]} /><Pager cursor={cursor} nextCursor={query.data.nextCursor} onPage={onPage} /></> : <Empty title="暂无显式空间成员" description="实例管理员仍具备所有空间的业务权限，不需要单独加入成员。" />}</QueryState></div>;
}
function WorkspaceInvitations() {
  const workspace = useWorkspace(); const client = useQueryClient(); const [secret, setSecret] = useState("");
  const query = useRPC(IdentityService.method.listInvitations, { workspaceId: workspace.id });
  const refresh = () => invalidateMethods(client, ["ListInvitations"]);
  return <div className="stack">{secret && <SecretOnce value={secret} title="邀请链接已创建" onDismiss={() => setSecret("")} />}<section className="section max-w-2xl"><h2 className="section-title">邀请成员</h2><p className="mb-6 text-sm text-muted-foreground">邀请 24 小时有效。已有用户名的成员必须使用该账户密码接受邀请。</p><DataForm fields={[{ name: "username", label: "受邀用户名", schema: required }, { name: "role", label: "空间角色", value: "member", options: [{ value: "member", label: "普通成员" }, { value: "admin", label: "空间管理员" }], schema: required }]} submitLabel="创建邀请链接" onSubmit={async (value) => { const response = await platform.identity.createInvitation({ workspaceId: workspace.id, username: value.username, role: value.role }); if (!response.token) throw new Error("控制面未返回邀请令牌"); setSecret(`${window.location.origin}/accept-invite#token=${encodeURIComponent(response.token)}`); await refresh(); }} /></section><QueryState query={query}>{query.data?.invitations.length ? <DataTable label="工作空间邀请" rows={query.data.invitations} columns={[
    { id: "name", title: "受邀账户", cell: (invitation) => invitation.username },
    { id: "role", title: "空间角色", cell: (invitation) => roleLabel(invitation.role) },
    { id: "time", title: "到期时间", cell: (invitation) => <span className="whitespace-nowrap text-xs">{formatTime(invitation.expiresAt)}</span> },
    { id: "action", title: "状态与操作", cell: (invitation) => invitation.consumedAt ? <Badge variant="secondary">已接受</Badge> : invitation.revokedAt ? <Badge variant="secondary">已撤销</Badge> : new Date(invitation.expiresAt).getTime() <= Date.now() ? <Badge variant="secondary">已过期</Badge> : <ConfirmAction label="撤销邀请" destructive description={`撤销发送给 ${invitation.username} 的邀请链接，之后将无法使用此链接加入。`} onConfirm={async () => { await platform.identity.revokeInvitation({ workspaceId: workspace.id, invitationId: invitation.id }); await refresh(); }} /> },
  ]} /> : <Empty title="还没有邀请" description="在上方创建邀请，通过安全渠道将一次性链接发送给团队成员。" />}</QueryState></div>;
}
