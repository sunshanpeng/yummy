import { useEffect, useRef, useState } from "react";
import { Link, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, LogOut, Settings2, Shield, UserRound, KeyRound, ArrowLeft } from "lucide-react";
import { Button } from "./components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "./components/ui/dropdown-menu";
import { Loading, Notice, QueryState } from "./components/console-kit";
import { WorkspaceService } from "./gen/platform/v1/resources_pb";
import { platform, errorMessage } from "./lib/platform";
import { ScopeContext, SessionContext, useRPC, useUser } from "./lib/queries";
import { sessionOptions } from "./session";

export function ConsoleRoot() {
  return <><a href="#main-content" className="sr-only absolute left-4 top-2 z-50 rounded bg-card px-4 py-2 focus:not-sr-only">跳转到主要内容</a><Outlet /></>;
}
export function AuthenticatedShell() {
  const query = useQuery(sessionOptions); const client = useQueryClient(); const navigate = useNavigate();
  const path = useRouterState({ select: (state) => state.location.pathname });
  const previous = useRef("");
  const scope = path.startsWith("/w/") ? `workspace:${path.split("/")[2]}` : path.startsWith("/platform") ? "platform" : path.startsWith("/account") ? "account" : "home";
  const [logoutError, setLogoutError] = useState("");
  useEffect(() => {
    const expire = () => { void client.cancelQueries(); client.clear(); platform.resetCsrf(); void navigate({ to: "/login", search: { redirect: path }, replace: true }); };
    window.addEventListener("yummy:session-expired", expire);
    return () => window.removeEventListener("yummy:session-expired", expire);
  }, [client, navigate, path]);
  useEffect(() => {
    if (query.data?.csrfToken) platform.setCsrfToken(query.data.csrfToken);
    if (query.data && !query.data.user) void navigate({ to: "/login", search: { redirect: path }, replace: true });
  }, [query.data, navigate, path]);
  useEffect(() => {
    const identity = query.data?.user ? `${query.data.user.id}:${query.data.user.authVersion}` : "";
    const key = `${identity}:${scope}`;
    if (previous.current && previous.current !== key) {
      const oldScope = { predicate: (cached: { queryKey: readonly unknown[] }) => cached.queryKey[0] === "scope" && (cached.queryKey[1] !== query.data?.user?.id || cached.queryKey[2] !== query.data?.user?.authVersion.toString() || cached.queryKey[3] !== scope) };
      void client.cancelQueries(oldScope);
      client.removeQueries(oldScope);
      client.getMutationCache().clear();
    }
    previous.current = key;
  }, [scope, query.data?.user?.id, query.data?.user?.authVersion, client]);
  if (query.isPending) return <Loading label="正在确认安全会话…" />;
  if (query.error) return <main className="page"><QueryState query={query}>{null}</QueryState></main>;
  if (!query.data?.user) return <Loading label="正在前往登录…" />;
  const user = query.data.user;
  return <SessionContext.Provider value={user}><ScopeContext.Provider value={scope}><header className="border-b bg-card"><div className="mx-auto flex min-h-16 max-w-[1440px] items-center justify-between gap-3 px-5 sm:px-8"><div className="flex min-w-0 items-center gap-3 sm:gap-6"><Link to="/" className="shrink-0 text-xl font-bold tracking-tight">yummy<span className="text-muted-foreground">.</span></Link>{scope.startsWith("workspace:") ? <WorkspaceSwitcher workspaceSlug={path.split("/")[2]} /> : <span className="truncate border-l pl-4 text-sm text-muted-foreground">{scope === "account" ? "个人账户" : scope === "platform" ? "全局管理" : "控制台"}</span>}</div><div className="flex shrink-0 items-center gap-2"><DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" className="max-w-44 gap-2 px-2" aria-label="账户菜单"><span className="flex size-7 items-center justify-center rounded-full bg-secondary"><UserRound className="size-4" /></span><span className="hidden truncate sm:inline">{user.username}</span><ChevronDown /></Button></DropdownMenuTrigger><DropdownMenuContent align="end" className="w-56"><DropdownMenuLabel className="truncate">{user.username}</DropdownMenuLabel><DropdownMenuSeparator /><DropdownMenuItem asChild><Link to="/account"><UserRound />个人账户</Link></DropdownMenuItem><DropdownMenuItem asChild><Link to="/account/tokens"><KeyRound />个人访问令牌</Link></DropdownMenuItem><DropdownMenuItem asChild><Link to="/account/connections"><KeyRound />Agent 连接</Link></DropdownMenuItem>{user.instanceAdmin && <DropdownMenuItem asChild><Link to="/platform/workspaces"><Shield />全局管理</Link></DropdownMenuItem>}<DropdownMenuSeparator /><DropdownMenuItem asChild><Link to="/"><ArrowLeft />返回工作空间</Link></DropdownMenuItem><DropdownMenuItem onSelect={() => { void (async () => { try { await platform.identity.logout({}); await client.cancelQueries(); client.clear(); platform.resetCsrf(); await navigate({ to: "/login" }); } catch (reason) { setLogoutError(errorMessage(reason)); } })(); }}><LogOut />退出登录</DropdownMenuItem></DropdownMenuContent></DropdownMenu></div></div></header>{logoutError && <div className="mx-auto max-w-[1320px] px-5 pt-4"><Notice tone="error">{logoutError}</Notice></div>}<main id="main-content" key={`${user.id}:${user.authVersion}:${scope}`}><Outlet /></main><footer className="mx-auto mt-10 max-w-[1320px] border-t px-5 py-6 text-xs text-muted-foreground sm:px-8 lg:px-12">Yummy 控制台 <span className="mx-2">/</span> 工作空间 → 应用</footer></ScopeContext.Provider></SessionContext.Provider>;
}
function WorkspaceSwitcher({ workspaceSlug }: { workspaceSlug: string }) {
  const [cursor, setCursor] = useState("");
  const query = useRPC(WorkspaceService.method.listWorkspaces, { limit: 24, cursor });
  const current = useRPC(WorkspaceService.method.getWorkspace, { slug: workspaceSlug });
  useUser();
  return <DropdownMenu><DropdownMenuTrigger asChild><Button variant="ghost" className="max-w-[min(48vw,260px)] gap-2 border-l pl-4" aria-label="切换工作空间"><span className="truncate">{current.data?.workspace?.name ?? "工作空间"}</span><ChevronDown /></Button></DropdownMenuTrigger><DropdownMenuContent align="start" className="w-64 max-w-[90vw]"><DropdownMenuLabel>工作空间</DropdownMenuLabel><DropdownMenuSeparator />{query.isPending ? <DropdownMenuItem disabled>正在加载…</DropdownMenuItem> : query.error ? <DropdownMenuItem onSelect={(event) => { event.preventDefault(); void query.refetch(); }}>加载失败，点击重试</DropdownMenuItem> : query.data?.workspaces.length ? query.data.workspaces.map((workspace) => <DropdownMenuItem key={workspace.id} asChild><Link to="/w/$workspaceSlug/apps" params={{ workspaceSlug: workspace.slug }} className="truncate">{workspace.name}{workspace.slug === workspaceSlug ? "（当前）" : ""}</Link></DropdownMenuItem>) : <DropdownMenuItem disabled>没有可访问空间</DropdownMenuItem>}{query.data?.nextCursor && <DropdownMenuItem onSelect={(event) => { event.preventDefault(); setCursor(query.data.nextCursor); }}>加载下一页</DropdownMenuItem>}{cursor && <DropdownMenuItem onSelect={(event) => { event.preventDefault(); setCursor(""); }}>返回首页</DropdownMenuItem>}<DropdownMenuSeparator /><DropdownMenuItem asChild><Link to="/">全部工作空间</Link></DropdownMenuItem>{current.data?.workspace?.capabilities.includes("workspace:write") && <DropdownMenuItem asChild><Link to="/w/$workspaceSlug/manage/settings" params={{ workspaceSlug }}><Settings2 />空间管理</Link></DropdownMenuItem>}</DropdownMenuContent></DropdownMenu>;
}
