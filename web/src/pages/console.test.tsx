import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { router } from "../router";
import { platform } from "../lib/platform";
import { sessionOptions } from "../session";
import { GetSessionResponseSchema } from "../gen/platform/v1/identity_pb";
import { SaveRuntimeConfigResponseSchema } from "../gen/platform/v1/runtime_config_pb";
import { Code, ConnectError } from "@connectrpc/connect";
import { UpdateRegistryResponseSchema } from "../gen/platform/v1/inventory_pb";

function mount(path: string, admin = false) {
  vi.spyOn(window, "scrollTo").mockImplementation(() => {});
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(sessionOptions.queryKey, create(GetSessionResponseSchema, { csrfToken: "csrf", user: { id: "user-current", username: "current", status: "ACTIVE", instanceAdmin: admin, authVersion: 1n } }));
  const instance = createRouter({ routeTree: router.options.routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  render(<QueryClientProvider client={client}><RouterProvider router={instance} /></QueryClientProvider>);
  return client;
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
describe("正式页面权限与作用域", () => {
  it("应用详情顶栏提供交互面包屑，不再渲染页内返回应用行", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read"] } }
        : method.name === "ListWorkspaces" ? { workspaces: [] }
        : method.name === "ListApplications" ? { applications: [{ id: "a1", name: "app1", capabilities: ["workspace:read"] }, { id: "a2", name: "app2", capabilities: [] }] }
        : method.name === "GetApplication" ? { application: { id: "a1", workspaceId: "w1", name: "app1", capabilities: ["workspace:read", "build:read"] } }
        : method.name === "ListBuilds" ? { builds: [] }
        : method.name === "ListDeployments" ? { operations: [] }
        : method.name === "GetRuntimeStatus" ? { status: { generation: 1n } }
        : {};
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    mount("/w/w1/apps/a1");
    // 空间名就位（GetWorkspace 返回后，面包屑空间段以空间名渲染且可点击）
    expect(await screen.findByRole("link", { name: "空间" })).toBeTruthy();
    expect(await screen.findByRole("button", { name: "切换应用" })).toBeTruthy();
    expect(screen.queryByText("返回应用")).toBeNull();
  });

  it("个人页不出现空间切换或空间管理", async () => {
    mount("/account");
    await screen.findByRole("heading", { name: /^个人账户$/ });
    expect(screen.queryByRole("button", { name: "切换工作空间" })).toBeNull();
    expect(screen.queryByText("空间管理")).toBeNull();
    expect(screen.getByRole("heading", { name: "修改密码" })).toBeTruthy();
  });
  it("普通账户直达全局页不请求用户列表", async () => {
    const list = vi.spyOn(platform.identity, "listUsers");
    mount("/platform/users");
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "此页面仅供全局管理员使用。");
    expect(list).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "切换工作空间" })).toBeNull();
  });
  it("创建应用重试复用幂等键，修改名称才生成新键", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read", "application:create"] } } : method.name === "ListWorkspaces" ? { workspaces: [] } : { applications: [] };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    const submit = vi.spyOn(platform.applications, "createApplication").mockRejectedValue(new Error("网络连接中断，结果尚未确认"));
    mount("/w/w1/apps"); const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "创建应用" }));
    await user.type(screen.getByLabelText("应用名称"), "first-app");
    await user.click(screen.getByRole("button", { name: "创建并配置" }));
    await screen.findByRole("alert");
    await user.click(screen.getByRole("button", { name: "创建并配置" }));
    expect(submit.mock.calls).toHaveLength(2);
    expect(submit.mock.calls[0][0].idempotencyKey).toBe(submit.mock.calls[1][0].idempotencyKey);
    await user.clear(screen.getByLabelText("应用名称"));
    await user.type(screen.getByLabelText("应用名称"), "second-app");
    await user.click(screen.getByRole("button", { name: "创建并配置" }));
    expect(submit.mock.calls[2][0].idempotencyKey).not.toBe(submit.mock.calls[0][0].idempotencyKey);
  });
  it("未配置的平台策略由NotFound进入首次配置而不是伪造现有策略", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      if (method.name === "GetExecutionPolicy") throw new ConnectError("尚未配置", Code.NotFound);
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, { nodes: [], registries: [] }) };
    });
    mount("/platform/policy", true);
    expect(await screen.findByText("实例尚未配置执行策略，请选择真实节点和输出仓库。")).toBeTruthy();
    expect(screen.getByRole("button", { name: "保存平台策略" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "切换工作空间" })).toBeNull();
  });
  it("仓库不替换凭据时完全省略optional凭据字段", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => ({ stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, { registries: [{ id: "registry-one", name: "output", url: "https://registry.example.com", status: "ACTIVE", revision: 2n, credentialConfigured: true, credentialVersion: 1n }] }) }));
    const save = vi.spyOn(platform.inventory, "updateRegistry").mockResolvedValue(create(UpdateRegistryResponseSchema, {}));
    mount("/platform/registries", true); const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "编辑" }));
    expect(screen.queryByLabelText("仓库密码 / 访问令牌")).toBeNull();
    await user.click(screen.getByRole("button", { name: "保存仓库设置" }));
    expect(save).toHaveBeenCalledOnce();
    expect(save.mock.calls[0][0]).not.toHaveProperty("username");
    expect(save.mock.calls[0][0]).not.toHaveProperty("password");
    expect(save.mock.calls[0][0]).toHaveProperty("expectedRevision", 2n);
  });
  it("运行配置保留敏感旧值且不发送掩码", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read"] } } : method.name === "ListWorkspaces" ? { workspaces: [] } : method.name === "GetApplication" ? { application: { id: "a1", workspaceId: "w1", name: "app", capabilities: ["config:read", "config:write"] } } : { config: { workspaceId: "w1", applicationId: "a1", revision: 2n, type: "HTTP", port: 8080, variables: [{ name: "API_KEY", sensitive: true, configured: true }], files: [{ path: "/app/secret", sensitive: true, configured: true }] } };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    const save = vi.spyOn(platform.runtime, "saveRuntimeConfig").mockResolvedValue(create(SaveRuntimeConfigResponseSchema, { config: { revision: 3n } }));
    mount("/w/w1/apps/a1/settings/runtime");
    const user = userEvent.setup();
    const variable = await screen.findByLabelText("变量 1 值") as HTMLInputElement;
    expect(variable.disabled).toBe(true);
    expect(variable.value).toBe("");
    await user.click(screen.getByRole("button", { name: "保存运行配置" }));
    expect(save).toHaveBeenCalledWith(expect.objectContaining({ expectedRevision: 2n, variables: [{ name: "API_KEY", value: "", sensitive: true, keepExisting: true }], files: [{ path: "/app/secret", content: "", sensitive: true, keepExisting: true }] }));
  });
  it("创建PAT显示一次性明文，关闭后不保留在查询缓存", async () => {
    // 列表和选择器通过真实 ConnectQuery 传输层响应，只有写 client 在此组件测试替换。
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "ListAccessTokens" ? { tokens: [] } : method.name === "ListWorkspaces" ? { workspaces: [{ id: "w1", name: "真实空间" }] } : { applications: [{ id: "a1", name: "my-app", capabilities: ["application:read", "build:read", "logs:read", "deployment:read", "runtime:read", "config:read"] }] };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    const createToken = vi.spyOn(platform.identity, "createAccessToken").mockResolvedValue({ $typeName: "platform.v1.CreateAccessTokenResponse", plaintext: "ym_pat_just-once" });
    const client = mount("/account/tokens");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "创建令牌" }));
    await user.type(screen.getByLabelText("令牌名称"), "local-cli");
    await user.selectOptions(await screen.findByLabelText("目标工作空间（仅用于选择授权应用）"), "w1");
    await user.click(await screen.findByRole("checkbox", { name: /my-app/ }));
    await user.click(screen.getByRole("button", { name: /^创建令牌$/ }));
    expect(await screen.findByText("ym_pat_just-once")).toBeTruthy();
    expect(createToken).toHaveBeenCalledWith(expect.objectContaining({ applicationIds: ["a1"], name: "local-cli" }));
    expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data), (_, value) => typeof value === "bigint" ? value.toString() : value)).not.toContain("ym_pat_just-once");
    await user.click(screen.getByRole("button", { name: "已保存，关闭" }));
    expect(screen.queryByText("ym_pat_just-once")).toBeNull();
  });
});
