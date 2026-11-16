import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { router } from "../router";
import { platform } from "../lib/platform";
import { sessionOptions } from "../session";
import { GetSessionResponseSchema } from "../gen/platform/v1/identity_pb";

type Message = Record<string, unknown>;
type Handler = (message: Message) => unknown;

// OAuth 客户端管理走 Connect 传输层；handlers 按方法名拦截，未处理的直接失败，避免测试悄悄拿到假数据。
function makeClient(overrides: Partial<Message> = {}): Message {
  return { clientId: "partner-app", name: "合作伙伴客户端", source: "pre-registered", redirectUris: ["https://client.example/callback", "http://127.0.0.1:8765/callback"], scopes: ["application:read", "offline_access"], resources: ["https://api.example.test/api"], active: true, createdAt: "2026-11-01T08:00:00Z", ...overrides };
}

function mount(handlers: Record<string, Handler>, admin = true) {
  vi.spyOn(window, "scrollTo").mockImplementation(() => {});
  const calls: Record<string, Message[]> = {};
  vi.spyOn(platform.transport, "unary").mockImplementation(async (method, _signal, _timeout, _header, message) => {
    const handler = handlers[method.name];
    (calls[method.name] ??= []).push(message as Message);
    if (!handler) throw new Error(`测试未处理方法 ${method.name}`);
    // handler 返回消息初始化值；此处受控断言绕过对每个具体 schema 的泛型展开。
    return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, handler(message as unknown as Message) as never) };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(sessionOptions.queryKey, create(GetSessionResponseSchema, { csrfToken: "csrf", user: { id: "user-current", username: "current", status: "active", instanceAdmin: admin, authVersion: 1n } }));
  const instance = createRouter({ routeTree: router.options.routeTree, history: createMemoryHistory({ initialEntries: ["/platform/oauth-clients"] }) });
  render(<QueryClientProvider client={client}><RouterProvider router={instance} /></QueryClientProvider>);
  return calls;
}

async function fillRegistration(user: ReturnType<typeof userEvent.setup>, redirectUris: string) {
  // userEvent 操作必须顺序执行，并发推进同一输入队列会互相干扰。
  await user.type(screen.getByLabelText("客户端 ID（client_id）"), "cli-agent");
  await user.type(screen.getByLabelText("客户端名称"), "CLI 客户端");
  await user.type(screen.getByLabelText("回调地址（每行一个）"), redirectUris);
  await user.type(screen.getByLabelText("资源（每行一个）"), "https://api.example.test/api");
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("全局管理 OAuth 客户端", () => {
  it("全局管理员可以看到客户端清单、来源、回调、范围与创建时间", async () => {
    mount({ ListOAuthClients: () => ({ clients: [makeClient()], nextCursor: "" }) });
    expect(await screen.findByText("partner-app")).toBeTruthy();
    expect(screen.getByText("合作伙伴客户端")).toBeTruthy();
    expect(screen.getByText("预登记")).toBeTruthy();
    expect(screen.getByText("https://client.example/callback")).toBeTruthy();
    expect(screen.getByText("http://127.0.0.1:8765/callback")).toBeTruthy();
    expect(screen.getByText("application:read · offline_access")).toBeTruthy();
    expect(screen.getByText("https://api.example.test/api")).toBeTruthy();
    expect(screen.getByText("已启用")).toBeTruthy();
    // created_at 按会话时区格式化；这里用同一 Intl 逻辑推导期望文本，不硬编码时区相关字面量。
    const expected = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date("2026-11-01T08:00:00Z"));
    expect(screen.getByText(expected)).toBeTruthy();
  });

  it("没有客户端时展示空态而不伪造数据", async () => {
    mount({ ListOAuthClients: () => ({ clients: [], nextCursor: "" }) });
    expect(await screen.findByText("尚无 OAuth 客户端")).toBeTruthy();
    expect(screen.queryByText("partner-app")).toBeNull();
  });

  it("列表加载失败展示可重试错误并成功恢复", async () => {
    const user = userEvent.setup();
    let failing = true;
    const calls = mount({ ListOAuthClients: () => { if (failing) throw new ConnectError("control plane unavailable", Code.Unavailable); return { clients: [makeClient()], nextCursor: "" }; } });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("暂时无法连接控制面");
    expect(screen.getByRole("button", { name: "重新加载" })).toBeTruthy();
    failing = false;
    await user.click(screen.getByRole("button", { name: "重新加载" }));
    expect(await screen.findByText("partner-app")).toBeTruthy();
    expect(calls.ListOAuthClients.length).toBeGreaterThan(1);
  });

  it("回调地址提示性校验拦截明显不合规提交，服务端为准", async () => {
    const user = userEvent.setup();
    const calls = mount({ ListOAuthClients: () => ({ clients: [], nextCursor: "" }) });
    await screen.findByText("尚无 OAuth 客户端");
    await user.click(screen.getByRole("button", { name: "注册客户端" }));
    await fillRegistration(user, "http://intranet.example/callback");
    await user.click(screen.getByRole("button", { name: "登记客户端" }));
    expect(await screen.findByText(/每个回调地址必须是 https/)).toBeTruthy();
    expect(calls.RegisterOAuthClient).toBeUndefined();
  });

  it("注册成功后失效列表缓存并展示新客户端", async () => {
    const user = userEvent.setup();
    let registered = false;
    const calls = mount({
      ListOAuthClients: () => registered
        ? { clients: [makeClient(), makeClient({ clientId: "cli-agent", name: "CLI 客户端", redirectUris: ["http://127.0.0.1:9/redirect"], scopes: [], resources: ["https://api.example.test/api"] })], nextCursor: "" }
        : { clients: [makeClient()], nextCursor: "" },
      RegisterOAuthClient: () => { registered = true; return { client: makeClient({ clientId: "cli-agent", name: "CLI 客户端" }) }; },
    });
    await screen.findByText("partner-app");
    await user.click(screen.getByRole("button", { name: "注册客户端" }));
    await fillRegistration(user, "http://127.0.0.1:9/redirect");
    await user.type(screen.getByLabelText("授权范围（空格或逗号分隔，可留空）"), "application:read");
    await user.click(screen.getByRole("button", { name: "登记客户端" }));
    expect(await screen.findByText("CLI 客户端")).toBeTruthy();
    expect(calls.ListOAuthClients.length).toBeGreaterThan(1);
    expect(calls.RegisterOAuthClient).toHaveLength(1);
    expect(calls.RegisterOAuthClient?.[0]).toEqual({ clientId: "cli-agent", name: "CLI 客户端", redirectUris: ["http://127.0.0.1:9/redirect"], scopes: ["application:read"], resources: ["https://api.example.test/api"] });
  });

  it("停用客户端需要二次确认，取消不生效，确认后明确撤销全部授权并刷新", async () => {
    const user = userEvent.setup();
    let active = true;
    const calls = mount({
      ListOAuthClients: () => ({ clients: [makeClient({ active })], nextCursor: "" }),
      SetOAuthClientActive: (message) => { active = Boolean(message.active); return { client: makeClient({ active }) }; },
    });
    await screen.findByText("partner-app");
    await user.click(screen.getByRole("button", { name: "停用客户端" }));
    // 二次确认弹窗明确告知停用即撤销全部授权；此时还没有发起请求。
    expect(screen.getByText(/停用「合作伙伴客户端」会立即撤销该客户端的全部既有授权/)).toBeTruthy();
    expect(calls.SetOAuthClientActive).toBeUndefined();
    await user.click(screen.getByRole("button", { name: "取消" }));
    expect(calls.SetOAuthClientActive).toBeUndefined();
    await user.click(screen.getByRole("button", { name: "停用客户端" }));
    await user.click(screen.getByRole("button", { name: "确认" }));
    await vi.waitFor(() => expect(calls.SetOAuthClientActive).toHaveLength(1));
    expect(calls.SetOAuthClientActive?.[0]).toEqual({ clientId: "partner-app", active: false });
    expect(await screen.findByText("已停用")).toBeTruthy();
    expect(screen.getByRole("button", { name: "启用客户端" })).toBeTruthy();
  });

  it("非全局管理员沿用全局页门控且不发起请求", async () => {
    const calls = mount({}, false);
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "此页面仅供全局管理员使用。");
    expect(calls.ListOAuthClients).toBeUndefined();
    expect(screen.queryByRole("button", { name: "注册客户端" })).toBeNull();
    expect(screen.queryByRole("button", { name: "停用客户端" })).toBeNull();
  });
});
