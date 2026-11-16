import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { z } from "zod";
import { IdentityService } from "../gen/platform/v1/identity_pb";
import { ApplicationService, WorkspaceService } from "../gen/platform/v1/resources_pb";
import { Button } from "../components/ui/button";
import { Badge } from "../components/ui/badge";
import { Label } from "../components/ui/label";
import { ConfirmAction, DataForm, DataTable, Empty, Heading, Notice, Pager, QueryState, SecretOnce, passwordSchema, required } from "../components/console-kit";
import { defaultTokenScopes, formatTime, platform, tokenScopes } from "../lib/platform";
import { invalidateMethods, useRPC, useUser } from "../lib/queries";

export function AccountPage({ tokens = false }: { tokens?: boolean }) {
  const user = useUser(); const client = useQueryClient();
  return <div className="page"><Heading title="个人账户" description="账户与个人凭证独立于工作空间；只有你能管理自己的访问令牌。" /><nav className="tabs-nav" aria-label="个人账户导航"><Link to="/account" aria-current={!tokens ? "page" : undefined}>账户与安全</Link><Link to="/account/tokens" aria-current={tokens ? "page" : undefined}>个人访问令牌</Link><Link to="/account/connections">Agent 连接</Link></nav>{tokens ? <TokensPage /> : <div className="grid items-start gap-8 lg:grid-cols-[1fr_1fr]"><section className="section"><h2 className="section-title">账户资料</h2><p className="mb-6 text-sm text-muted-foreground">本地身份资料由实例管理，当前不支持修改用户名。</p><dl className="metadata"><div><dt>用户名</dt><dd>{user.username}</dd></div><div><dt>账户类型</dt><dd>{user.instanceAdmin ? "全局管理员" : "普通用户"}</dd></div><div><dt>账户 ID</dt><dd><code>{user.id}</code></dd></div><div><dt>状态</dt><dd>{user.status === "ACTIVE" ? "正常" : user.status}</dd></div></dl></section><section className="section"><h2 className="section-title">修改密码</h2><p className="mb-6 text-sm text-muted-foreground">成功后现有会话和个人访问令牌将失效，需要重新登录。</p><DataForm fields={[{ name: "oldPassword", label: "当前密码", type: "password", schema: required, autoComplete: "current-password" }, { name: "newPassword", label: "新密码", type: "password", schema: passwordSchema, hint: "12～128 个字节。", autoComplete: "new-password" }, { name: "confirm", label: "确认新密码", type: "password", schema: required, autoComplete: "new-password" }]} submitLabel="更新密码并重新登录" onSubmit={async (values) => { await platform.identity.changePassword(values); await client.cancelQueries(); client.clear(); window.location.assign("/login"); }} /></section></div>}</div>;
}
function TokensPage() {
  const tokens = useRPC(IdentityService.method.listAccessTokens, {});
  const client = useQueryClient(); const [creating, setCreating] = useState(false); const [secret, setSecret] = useState("");
  const refresh = () => invalidateMethods(client, ["ListAccessTokens"]);
  return <div className="stack"><div className="flex flex-wrap items-center justify-between gap-4"><div><h2 className="text-lg font-semibold">个人访问令牌</h2><p className="mt-1 text-sm text-muted-foreground">为 CLI 和 Agent 授权指定应用。权限会随当前账户和应用授权收回。</p></div><Button onClick={() => { setCreating(!creating); setSecret(""); }} variant={creating ? "outline" : "default"}><Plus />{creating ? "取消创建" : "创建令牌"}</Button></div>{secret && <SecretOnce value={secret} title="个人访问令牌已创建" onDismiss={() => setSecret("")} />}{creating && <TokenCreate onCreated={async (value) => { setSecret(value); setCreating(false); await refresh(); }} />}
  <QueryState query={tokens}>{tokens.data?.tokens.length ? <DataTable label="个人访问令牌" rows={tokens.data.tokens} columns={[
    { id: "name", title: "令牌", cell: (token) => <div><p className="font-medium">{token.name}</p><p className="mt-1 text-xs text-muted-foreground">创建于 {formatTime(token.createdAt)}</p></div> },
    { id: "scope", title: "授权范围", cell: (token) => <div className="max-w-sm"><p className="break-words text-xs">{token.scopes.join(" · ")}</p><p className="mt-1 break-all text-xs text-muted-foreground">应用：{token.applicationIds.join("、")}</p></div> },
    { id: "expires", title: "到期 / 最近使用", cell: (token) => <div className="whitespace-nowrap text-xs">{formatTime(token.expiresAt)}<p className="mt-1 text-muted-foreground">最近使用：{formatTime(token.lastUsedAt)}</p></div> },
    { id: "action", title: "状态与操作", cell: (token) => token.revokedAt ? <Badge variant="secondary">已撤销</Badge> : new Date(token.expiresAt).getTime() <= Date.now() ? <Badge variant="secondary">已过期</Badge> : <ConfirmAction label="撤销" destructive description={`撤销「${token.name}」后，使用该凭证的 CLI 或 Agent 将无法继续请求。不会自动取消已有任务。`} onConfirm={async () => { await platform.identity.revokeAccessToken({ tokenId: token.id }); await refresh(); }} /> },
  ]} /> : <Empty title="还没有个人访问令牌" description="需要通过 CLI 或 Agent 访问应用时，再创建最小必要权限的凭证。" />}</QueryState></div>;
}
function TokenCreate({ onCreated }: { onCreated: (plaintext: string) => Promise<void> }) {
  const [workspaceId, setWorkspaceId] = useState(""); const [workspaceCursor, setWorkspaceCursor] = useState(""); const [appCursor, setAppCursor] = useState("");
  const [apps, setApps] = useState<string[]>([]); const [scopes, setScopes] = useState(defaultTokenScopes);
  const workspaces = useRPC(WorkspaceService.method.listWorkspaces, { limit: 24, cursor: workspaceCursor });
  const applications = useRPC(ApplicationService.method.listApplications, { workspaceId, limit: 24, cursor: appCursor }, { enabled: !!workspaceId });
  const [capabilities, setCapabilities] = useState<Record<string, string[]>>({});
  const allowed = tokenScopes.filter((scope) => apps.every((id) => capabilities[id]?.includes(scope)));
  return <section className="section max-w-3xl"><h3 className="section-title">创建个人访问令牌</h3><p className="mb-6 text-sm text-muted-foreground">仅授予你对所选每个应用都具备的权限，最长有效 90 天。不会授予空间或实例管理权限。</p><DataForm submitLabel="创建令牌" fields={[{ name: "name", label: "令牌名称", schema: required.max(100, "最多 100 字符") }, { name: "days", label: "有效期", value: "30", options: [{ value: "7", label: "7 天" }, { value: "30", label: "30 天" }, { value: "90", label: "90 天" }], schema: z.enum(["7", "30", "90"]) }]} onSubmit={async (value) => {
    if (!apps.length) throw new Error("请至少选择一个应用。");
    const selectedScopes = scopes.filter((scope) => allowed.includes(scope));
    if (!selectedScopes.length) throw new Error("请至少选择一项当前有权授予的权限。");
    const response = await platform.identity.createAccessToken({ name: value.name, applicationIds: apps, scopes: selectedScopes, expiresAt: new Date(Date.now() + Number(value.days) * 86400000).toISOString() });
    if (!response.plaintext) throw new Error("服务端没有返回令牌明文，请检查令牌列表后再决定是否重试。");
    await onCreated(response.plaintext);
  }}>
    <div className="space-y-3"><Label htmlFor="token-workspace">目标工作空间（仅用于选择授权应用）</Label><QueryState query={workspaces}><select id="token-workspace" className="w-full" value={workspaceId} onChange={(event) => { setWorkspaceId(event.target.value); setAppCursor(""); }}><option value="">选择工作空间</option>{workspaces.data?.workspaces.map((w) => <option value={w.id} key={w.id}>{w.name}</option>)}</select><Pager cursor={workspaceCursor} nextCursor={workspaces.data?.nextCursor} onPage={setWorkspaceCursor} /></QueryState></div>
    {workspaceId && <fieldset className="space-y-3"><legend className="mb-2 text-sm font-medium">授权应用</legend><QueryState query={applications}>{applications.data?.applications.length ? applications.data.applications.map((app) => <label key={app.id} className="flex items-start gap-3 rounded-md border p-3"><input type="checkbox" className="mt-1 size-4 accent-primary" checked={apps.includes(app.id)} onChange={(event) => { setCapabilities((previous) => ({ ...previous, [app.id]: app.capabilities })); setApps((previous) => event.target.checked ? [...previous, app.id] : previous.filter((id) => id !== app.id)); }} /><span>{app.name}<span className="block break-all text-xs text-muted-foreground">{app.id}</span></span></label>) : <Notice>此空间没有可访问的应用。</Notice>}<Pager cursor={appCursor} nextCursor={applications.data?.nextCursor} onPage={setAppCursor} /></QueryState></fieldset>}
    {!!apps.length && <div className="rounded-md bg-muted p-3 text-xs">已选 {apps.length} 个应用：<span className="break-all">{apps.join("、")}</span><Button type="button" variant="link" size="sm" onClick={() => setApps([])}>清空选择</Button></div>}
    <fieldset><legend className="mb-3 text-sm font-medium">凭证权限</legend><div className="grid gap-3 sm:grid-cols-2">{tokenScopes.map((scope) => <label key={scope} className={`flex items-center gap-2 text-xs ${!allowed.includes(scope) ? "text-muted-foreground" : ""}`}><input className="size-4 accent-primary" type="checkbox" checked={scopes.includes(scope) && allowed.includes(scope)} disabled={!apps.length || !allowed.includes(scope)} onChange={(event) => setScopes((previous) => event.target.checked ? [...previous, scope] : previous.filter((item) => item !== scope))} />{scope}</label>)}</div><p className="mt-3 text-xs text-muted-foreground">先选应用；不属于所有已选应用 capabilities 的权限不可授予。</p></fieldset>
  </DataForm></section>;
}
