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
import { UpdateRegistryResponseSchema, VerifyRegistryResponseSchema } from "../gen/platform/v1/inventory_pb";

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
  it("只允许删除无执行历史的归档应用，并在确认后刷新结果", async () => {
    let deleted = false;
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method, _signal, _timeout, _header, input) => {
      const archived = method.name === "ListApplications" && "archivedOnly" in input && input.archivedOnly;
      if (method.name === "DeleteApplication") {
        expect(input).toMatchObject({ workspaceId: "w1", applicationId: "unused", expectedRevision: 2n });
        deleted = true;
      }
      const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read", "workspace:write"] } }
        : method.name === "ListWorkspaces" ? { workspaces: [] }
        : archived ? { applications: [{ id: "built", name: "built-app" }, ...(!deleted ? [{ id: "unused", name: "unused-app", revision: 2n, deletionReady: true }] : [])] }
        : { applications: [] };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    mount("/w/w1/apps");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "已归档应用" }));
    expect(await screen.findByText("待清理集群资源")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "删除应用" }));
    expect(deleted).toBe(false);
    await user.click(screen.getByRole("button", { name: "确认" }));
    expect(await screen.findByText("已删除「unused-app」，可重新使用该名称。")).toBeTruthy();
    await screen.findByText("built-app");
    expect(screen.queryByRole("button", { name: "删除应用" })).toBeNull();
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
  it("全局接入检查读取策略选定的节点和仓库，并区分入口配置与验证", async () => {
    const calls = vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values: Record<string, Record<string, unknown>> = {
        GetExecutionPolicy: { policy: { revision: 1n, buildNodeId: "b1", runtimeNodeId: "r1", registryId: "reg1", baseDomain: "apps.example.test" } },
        GetNode: { node: { status: "ACTIVE", os: "linux", arch: "amd64", clusterNodeUid: "k8s-node", lastSeen: new Date().toISOString(), buildReady: true, runtimeReady: true, ingressReady: true, metricsReady: true } },
        GetRegistry: { registry: { id: "reg1", name: "输出", status: "ACTIVE" } },
        ListWorkspaces: { workspaces: [{ id: "w1", name: "验收空间", slug: "demo" }] },
        ListApplications: { applications: [{ id: "app1", name: "gateway" }] },
        GetRuntimeStatus: { status: { observedState: "NOT_DEPLOYED" } },
        GetWorkspaceAllocation: { allocation: { allowedNodeIds: ["b1", "r1"], revision: 1n } },
      };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values[method.name] ?? {}) };
    });
    mount("/platform/workspaces", true);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "接入检查" }));
    expect(await screen.findByText("输出仓库尚未验证，请先验证连接。")).toBeTruthy();
    expect(await screen.findByText("apps.example.test 已配置，等待 HTTP 应用的实际入口探测。")).toBeTruthy();
    expect((await screen.findByRole("link", { name: "前往应用部署" })).getAttribute("href")).toBe("/w/demo/apps/gateway/deployments");
    expect(calls.mock.calls.filter(([method]) => method.name === "GetNode").map((entry) => entry[4])).toEqual(expect.arrayContaining([
      expect.objectContaining({ nodeId: "b1" }), expect.objectContaining({ nodeId: "r1" }),
    ]));
  });

  it("仓库验证使用当前修订，按服务端结果展示失败与重试", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => ({
      stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(),
      message: create(method.output, { registries: [{ id: "reg-a", name: "测试仓库", status: "ACTIVE", revision: 4n }] }),
    }));
    const verify = vi.spyOn(platform.inventory, "verifyRegistry")
      .mockResolvedValueOnce(create(VerifyRegistryResponseSchema, { message: "认证失败" }))
      .mockResolvedValueOnce(create(VerifyRegistryResponseSchema, { ok: true }));
    mount("/platform/registries", true);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "验证连接" }));
    const submit = screen.getByRole("button", { name: "开始验证" });
    await user.click(submit);
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "认证失败");
    expect(verify).toHaveBeenCalledWith({ registryId: "reg-a", expectedRevision: 4n, readReference: "" });
    await user.click(submit);
    expect(await screen.findByText("仓库连接和认证已通过。")).toBeTruthy();
  });
  it("仅向空间审计传入当前路由对应的空间 ID", async () => {
    const request = vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace"
        ? { workspace: { id: "space-id", slug: "space-name", name: "测试空间", capabilities: ["workspace:write"] } }
        : method.name === "ListAuditEvents"
          ? { events: [{ id: "evt-a", action: "application.archive", objectId: "app-a" }] }
          : {};
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    mount("/w/space-name/manage/audit");
    expect(await screen.findByText("application.archive")).toBeTruthy();
    const call = request.mock.calls.find(([method]) => method.name === "ListAuditEvents");
    expect(call?.[4]).toMatchObject({ workspaceId: "space-id", limit: 24 });
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
  it("导入变量只预览键名，保留既有敏感值并将新变量加入未保存草稿", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace"
        ? { workspace: { id: "w1", slug: "w1", name: "空间" } }
        : method.name === "GetApplication"
          ? { application: { id: "a1", workspaceId: "w1", name: "app1", capabilities: ["config:read", "config:write"] } }
          : method.name === "GetRuntimeConfig"
            ? { config: { revision: 1n, type: "HTTP", port: 8080, variables: [{ name: "TOKEN", sensitive: true, configured: true }] } }
            : {};
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    const save = vi.spyOn(platform.runtime, "saveRuntimeConfig").mockResolvedValue(create(SaveRuntimeConfigResponseSchema, { config: { revision: 2n } }));
    mount("/w/w1/apps/app1/deployments/runtime-config");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "导入 .env" }));
    await user.type(screen.getByLabelText(".env 内容"), "TOKEN=should-not-replace{Enter}NEW_TOKEN=new-secret");
    await user.click(screen.getByRole("button", { name: "预览导入" }));
    expect(screen.getByText("保留敏感值")).toBeTruthy();
    expect(screen.getByText("新增 · 敏感")).toBeTruthy();
    expect(screen.queryByText("new-secret")).toBeNull();
    await user.click(screen.getByRole("button", { name: "应用到表单" }));
    expect(save).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "保存运行配置" }));
    expect(save).toHaveBeenCalledWith(expect.objectContaining({ variables: [
      { name: "TOKEN", value: "", sensitive: true, keepExisting: true },
      { name: "NEW_TOKEN", value: "new-secret", sensitive: true, keepExisting: false },
    ] }));
  });
  it("首次进入运行配置提供语言模板与必填标注", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
      const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read"] } } : method.name === "ListWorkspaces" ? { workspaces: [] } : method.name === "GetApplication" ? { application: { id: "a1", workspaceId: "w1", name: "app", capabilities: ["config:read", "config:write"] } } : method.name === "ResolveExecution" ? { summary: { policyRevision: 1n, ready: true, platform: "linux/amd64" } } : { config: { workspaceId: "w1", applicationId: "a1", revision: 0n, type: "HTTP", port: 8080 } };
      return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
    });
    mount("/w/w1/apps/a1/settings/runtime");
    const user = userEvent.setup();
    // 首次进入（尚未保存配置）展示按语言/形态分组的模板卡片
    expect(await screen.findByText("从模板开始")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /Node.js 服务/ }));
    expect(await screen.findByText(/已按「Node.js 服务」预填/)).toBeTruthy();
    // 必填字段有视觉标记与 aria-required；提示说明默认值
    const port = await screen.findByLabelText(/容器监听端口/) as HTMLInputElement;
    expect(port.getAttribute("aria-required")).toBe("true");
    expect(port.value).toBe("3000");
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
