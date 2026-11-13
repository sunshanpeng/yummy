import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearch } from "@tanstack/react-router";
import { Button } from "../components/ui/button";
import { ConfirmAction, Empty, Heading, Notice, QueryState } from "../components/console-kit";
import { formatTime, platform } from "../lib/platform";
import { useUser } from "../lib/queries";

type Choice = { id: string; workspaceId: string; name: string };
type Consent = { requestId: string; csrfToken: string; formCsrfToken: string; clientName: string; clientId: string; clientSource: string; resource: string; scopes: string[]; applications: string[] | null; availableApplications: Choice[] | null; offlineAccess: boolean };
const validRequest = (value: string) => /^req_[A-Za-z0-9_-]{20,100}$/.test(value);

export function ConnectionsPage() {
  const user = useUser(); const client = useQueryClient();
  const key = ["oauth-connections", user.id, user.authVersion.toString()];
  const query = useQuery({ queryKey: key, queryFn: ({ signal }) => platform.listConnections(signal), retry: false });
  return <main className="page"><Heading title="个人账户" description="Agent 连接由当前账户拥有，授权范围限定到你有权访问的应用。" /><nav className="tabs-nav" aria-label="个人账户导航"><Link to="/account">账户与安全</Link><Link to="/account/tokens">个人访问令牌</Link><Link to="/account/connections" aria-current="page">Agent 连接</Link></nav><div className="stack"><div><h2 className="text-lg font-semibold">Agent 连接</h2><p className="mt-1 text-sm text-muted-foreground">撤销只阻止后续请求，不会取消已受理的构建或部署。</p></div><QueryState query={query}>{query.data?.connections.length ? <ul className="space-y-3">{query.data.connections.map((connection) => <li key={connection.id} className="section space-y-3"><div className="flex flex-wrap items-start justify-between gap-3"><div><h3 className="break-all font-medium">{connection.clientId}</h3><p className="mt-1 text-xs text-muted-foreground">创建于 {formatTime(connection.createdAt)} · 到期 {formatTime(connection.expiresAt)} · {connection.revoked ? "已撤销" : "未撤销"}</p></div>{!connection.revoked && <ConfirmAction label="撤销连接" title="撤销此 Agent 连接？" description="此客户端后续请求将被拒绝；已开始的构建和部署不会因此取消。" destructive onConfirm={async () => { await platform.revokeConnection(connection.id); await client.invalidateQueries({ queryKey: key }); }} />}</div><p className="break-all text-xs text-muted-foreground">资源：{connection.resource}</p><p className="break-all text-xs text-muted-foreground">应用：{connection.applicationIds.join("、")}</p><p className="break-all text-xs text-muted-foreground">权限：{connection.scopes.join(" · ")}</p></li>)}</ul> : <Empty title="暂无 Agent 连接" description="从支持 OAuth 的客户端发起连接，完成授权后会在这里显示。" />}</QueryState></div></main>;
}

export function OAuthConsentPage() {
  const user = useUser();
  const search = useSearch({ strict: false }) as { request_id?: string };
  const id = search.request_id ?? "";
  const [apps, setApps] = useState<string[]>([]);
  const [offline, setOffline] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [finished, setFinished] = useState("");
  const query = useQuery({ queryKey: ["oauth-consent", user.id, user.authVersion.toString(), id], enabled: validRequest(id), retry: false, staleTime: Infinity, queryFn: async ({ signal }): Promise<Consent> => {
    const response = await fetch(`/oauth/consent?request_id=${encodeURIComponent(id)}`, { credentials: "same-origin", cache: "no-store", signal });
    if (!response.ok) throw new Error("授权请求无效、已过期或不属于当前账户。请返回客户端重新发起。");
    return response.json() as Promise<Consent>;
  } });
  async function decide(allow: boolean) {
    const view = query.data;
    if (!view || pending) return;
    const selected = view.applications?.length ? view.applications : apps;
    if (allow && !selected.length) { setError("请至少选择一个应用，或拒绝本次授权。"); return; }
    setPending(true); setError("");
    const form = new FormData();
    form.set("request_id", view.requestId); form.set("csrf_token", view.csrfToken);
    form.set("gorilla.csrf.Token", view.formCsrfToken);
    form.set("decision", allow ? "allow" : "deny");
    if (allow && offline) form.set("offline_access", "true");
    if (allow) selected.forEach((app) => form.append("application_id", app));
    try {
      // 回调只由服务端已验证的请求生成；浏览器不能用 fetch 跟随跨域授权重定向。
      const response = await fetch("/oauth/consent", { method: "POST", credentials: "same-origin", redirect: "error", headers: { "X-CSRF-Token": view.formCsrfToken, "X-Yummy-Response-Mode": "manual" }, body: form });
      if (!response.ok) throw new Error("授权未完成：请求已失效或当前账户不具备所选权限。请重新发起授权。");
      const result = await response.json() as { redirect?: string };
      const target = result.redirect ? new URL(result.redirect) : undefined;
      if (!target || (target.protocol !== "https:" && !(target.protocol === "http:" && ["127.0.0.1", "[::1]", "localhost"].includes(target.hostname))) || target.username || target.password) throw new Error("授权回调地址无效，请返回客户端重新发起。");
      setFinished("正在返回客户端。如果没有自动跳转，请返回客户端重新发起授权。");
      window.location.assign(target.href);
    } catch (reason) { setError(reason instanceof Error ? reason.message : "授权请求未完成，请重试。"); }
    finally { setPending(false); }
  }
  if (!validRequest(id)) return <main className="page"><Notice tone="error">授权请求无效，请从客户端重新发起。</Notice></main>;
  return <main className="page max-w-3xl"><Heading title="确认 Agent 授权" description="核对客户端、应用和请求的操作后再决定是否允许。" />
    <QueryState query={query}>{finished ? <Notice tone="success">{finished}</Notice> : query.data && <div className="stack">
      <section className="section"><h2 className="section-title">授权详情</h2><dl className="metadata"><div><dt>当前账户</dt><dd>{user.username}</dd></div><div><dt>客户端</dt><dd className="break-all">{query.data.clientName} · {query.data.clientId}</dd></div><div><dt>客户端来源</dt><dd>{query.data.clientSource === "cimd-unverified-publisher" ? "第三方描述，发布者未经验证" : query.data.clientSource}</dd></div><div><dt>目标资源</dt><dd className="break-all">{query.data.resource}</dd></div></dl></section>
      <section className="section"><h2 className="section-title">应用范围</h2>{query.data.applications?.length ? <p className="text-sm break-all">此连接仅限应用 {query.data.applications.join("、")}。</p> : query.data.availableApplications?.length ? <fieldset><legend className="mb-3 text-sm">选择客户端可以访问的应用</legend><div className="space-y-3">{query.data.availableApplications.map((app) => <label key={app.id} className="flex items-start gap-3 text-sm"><input type="checkbox" className="mt-0.5 size-4 accent-primary" checked={apps.includes(app.id)} onChange={(event) => setApps((items) => event.target.checked ? [...items, app.id] : items.filter((item) => item !== app.id))} /><span>{app.name}<span className="block break-all text-xs text-muted-foreground">工作空间 {app.workspaceId} · 应用 {app.id}</span></span></label>)}</div></fieldset> : <Notice>当前没有可供授权的应用。请联系空间管理员分配权限。</Notice>}</section>
      <section className="section"><h2 className="section-title">请求的操作</h2><ul className="space-y-1 text-sm">{query.data.scopes.filter((scope) => scope !== "offline_access").map((scope) => <li key={scope}><code>{scope}</code></li>)}</ul>{query.data.offlineAccess && <label className="mt-5 flex items-start gap-3 text-sm"><input className="mt-0.5 size-4 accent-primary" type="checkbox" checked={offline} onChange={(event) => setOffline(event.target.checked)} /><span>允许离线访问<span className="block text-xs text-muted-foreground">单独同意后客户端可刷新凭证；你可以随时撤销连接。</span></span></label>}</section>
      {error && <Notice tone="error">{error}</Notice>}<div className="flex flex-wrap gap-3"><Button onClick={() => void decide(true)} disabled={pending}>允许授权</Button><Button variant="outline" onClick={() => void decide(false)} disabled={pending}>拒绝</Button></div>
    </div>}</QueryState>
  </main>;
}
