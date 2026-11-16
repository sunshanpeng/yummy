import { useState } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ArrowUpRight } from "lucide-react";
import { DataForm, Notice, passwordSchema, required } from "../components/console-kit";
import { platform } from "../lib/platform";
import { sessionOptions } from "../session";

export function AuthPage({ mode }: { mode: "login" | "invite" | "reset" }) {
  const navigate = useNavigate(); const client = useQueryClient();
  const search = useSearch({ strict: false }) as { token?: string; redirect?: string; return_request?: string };
  const [completed, setCompleted] = useState(false);
  const initialToken = new URLSearchParams(window.location.hash.slice(1)).get("token") ?? search.token ?? "";
  const title = mode === "login" ? "登录 Yummy" : mode === "invite" ? "加入工作空间" : "重置密码";
  return <main id="main-content" className="mx-auto grid min-h-dvh w-full max-w-6xl items-center gap-12 px-6 py-12 md:grid-cols-[1fr_400px] md:gap-20 lg:px-12">
    <section className="self-start md:self-center"><Link to="/" className="inline-block text-2xl font-bold tracking-tight">yummy<span className="text-muted-foreground">.</span></Link><div className="mt-12 hidden max-w-md md:block"><p className="mb-4 text-xs font-medium tracking-[0.16em] text-muted-foreground">你的应用，从这里开始</p><h1 className="text-4xl font-semibold leading-tight tracking-tight">专注应用。<br />让交付有条不紊。</h1><p className="mt-6 max-w-sm text-base leading-relaxed text-muted-foreground">统一管理你的工作空间、应用与访问权限。每一步操作，都连接真实的控制面。</p><div className="mt-12 border-t pt-5 text-xs text-muted-foreground">YUMMY · APPLICATION CONSOLE</div></div></section>
    <section className="rounded-xl border bg-card p-6 sm:p-8"><h2 className="text-2xl font-semibold tracking-tight">{title}</h2><p className="mb-7 mt-2 text-sm text-muted-foreground">{mode === "login" ? "使用全局管理员或团队提供的本地账户。" : mode === "invite" ? "新账户请设置密码；已有账户请输入该账户当前密码，以确认本人接受邀请。" : "使用管理员提供的一次性重置令牌，设置新密码。"}</p>
      {completed ? <div className="space-y-5"><Notice tone="success">{mode === "invite" ? "邀请已接受。请登录后进入工作空间。" : "密码已重置。请使用新密码登录。"}</Notice><Link className="text-link inline-flex items-center gap-1" to="/login">前往登录<ArrowUpRight className="size-4" /></Link></div> : <DataForm key={mode} submitLabel={mode === "login" ? "登录" : mode === "invite" ? "接受邀请" : "重置密码"} fields={mode === "login" ? [
        { name: "username", label: "用户名", schema: required, autoComplete: "username" },
        { name: "password", label: "密码", type: "password", schema: required, autoComplete: "current-password" },
      ] : [
        { name: "token", label: mode === "invite" ? "邀请令牌" : "重置令牌", value: initialToken, schema: required, autoComplete: "off" },
        { name: "password", label: mode === "invite" ? "账户密码" : "新密码", type: "password", schema: passwordSchema, hint: "新密码必须为 12～128 个字节。", autoComplete: mode === "invite" ? "current-password" : "new-password" },
        ...(mode === "reset" ? [{ name: "confirm", label: "确认新密码", type: "password", schema: required, autoComplete: "new-password" }] : []),
      ]} onSubmit={async (value) => {
        if (mode === "login") {
          await platform.identity.login({ username: value.username, password: value.password });
          await client.cancelQueries(); client.clear();
          const session = await client.fetchQuery(sessionOptions);
          if (!session.user) throw new Error("登录完成但未建立会话，请检查浏览器 Cookie 设置。");
          if (search.return_request && /^req_[A-Za-z0-9_-]{20,100}$/.test(search.return_request)) {
            await navigate({ to: "/authorize", search: { request_id: search.return_request } });
          } else {
            await navigate({ to: search.redirect?.startsWith("/") && !search.redirect.startsWith("//") && !search.redirect.startsWith("/login") ? search.redirect : "/" });
          }
        } else {
          if (mode === "invite") await platform.identity.acceptInvitation({ token: value.token, password: value.password });
          else await platform.identity.completePasswordReset({ token: value.token, newPassword: value.password });
          setCompleted(true);
          // 移除 URL 中的一次性凭证，避免保留在可分享链接中。
          await navigate({ to: mode === "invite" ? "/accept-invite" : "/reset-password", replace: true });
        }
      }} />}
      {mode === "login" ? <div className="mt-7 border-t pt-5 text-xs text-muted-foreground">没有账户或忘记密码？请联系实例或工作空间管理员。</div> : <Link className="mt-7 inline-flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground" to="/login"><ArrowLeft className="size-4" />返回登录</Link>}
    </section>
  </main>;
}
