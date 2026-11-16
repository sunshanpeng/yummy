import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { router } from "../router";
import { platform } from "../lib/platform";
import { sessionOptions } from "../session";
import { GetSessionResponseSchema } from "../gen/platform/v1/identity_pb";
import { PreviewBuildResponseSchema, ReadBuildLogsResponseSchema } from "../gen/platform/v1/build_pb";
import { GetRuntimeStatusResponseSchema } from "../gen/platform/v1/deployment_pb";
import { GetRuntimeConfigResponseSchema } from "../gen/platform/v1/runtime_config_pb";
import { ResolveExecutionResponseSchema } from "../gen/platform/v1/inventory_pb";

const capabilities = ["application:read", "source:read", "source:upload", "build:read", "build:write", "logs:read", "config:read", "config:write", "deployment:read", "deployment:write", "runtime:read", "runtime:write", "secret:write"];
function mount(path: string) {
  vi.spyOn(window, "scrollTo").mockImplementation(() => {});
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(sessionOptions.queryKey, create(GetSessionResponseSchema, { csrfToken: "csrf", user: { id: "u1", username: "user", status: "ACTIVE", authVersion: 1n } }));
  const instance = createRouter({ routeTree: router.options.routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  render(<QueryClientProvider client={client}><RouterProvider router={instance} /></QueryClientProvider>);
  return client;
}
function transport() {
  vi.spyOn(platform.runtime, "getRuntimeConfig").mockResolvedValue(create(GetRuntimeConfigResponseSchema, { config: { revision: 5n, confirmed: true } }));
  vi.spyOn(platform.deployments, "getRuntimeStatus").mockResolvedValue(create(GetRuntimeStatusResponseSchema, { status: { generation: 3n, servingReleaseId: "release-old" } }));
  return vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => {
    const values = method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1", name: "空间", capabilities: ["workspace:read"] } }
      : method.name === "ListWorkspaces" ? { workspaces: [] }
      : method.name === "GetApplication" ? { application: { id: "a1", workspaceId: "w1", name: "my-app", capabilities } }
      : method.name === "GetBuildConfig" ? { config: { dockerfile: "Dockerfile", context: "." }, revision: 2n }
      : method.name === "GetRuntimeConfig" ? { config: { revision: 5n, confirmed: true, type: "HTTP", port: 8080 } }
      : method.name === "GetRuntimeStatus" ? { status: { workspaceId: "w1", applicationId: "a1", generation: 3n, servingReleaseId: "release-old", observedGeneration: 3n, observedState: "RUNNING" } }
      : method.name === "ResolveExecution" ? { summary: { policyRevision: 7n, ready: true, platform: "linux/amd64", imageRepository: "registry.example.com/a1" } }
      : method.name === "GetBuild" ? { build: { id: "b1", workspaceId: "w1", applicationId: "a1", state: "FAILED", sourceSnapshotId: "src1", configRevision: 2n, policyRevision: 7n } }
      : {};
    return { stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, values) };
  });
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
describe("构建与发布冻结交互", () => {
  it("预览使用业务策略摘要且修改输入后必须重新预览，不提前创建构建", async () => {
    transport();
    const preview = vi.spyOn(platform.builds, "previewBuild").mockResolvedValue(create(PreviewBuildResponseSchema, { preview: { sourceSnapshotId: "src1", sourceSha256: "sha256-source", execution: { policyRevision: 7n, ready: true, platform: "linux/amd64" } } }));
    const createBuild = vi.spyOn(platform.builds, "createBuild");
    mount("/w/w1/apps/a1/builds/new"); const user = userEvent.setup();
    await user.type(await screen.findByLabelText("源码快照 ID"), "src1");
    expect((screen.getByLabelText("预期平台策略版本（只读）") as HTMLInputElement).disabled).toBe(true);
    await user.click(screen.getByRole("button", { name: "预览构建输入" }));
    await screen.findByRole("heading", { name: "确认冻结输入" });
    expect(preview).toHaveBeenCalledWith(expect.objectContaining({ workspaceId: "w1", applicationId: "a1", input: expect.objectContaining({ configRevision: 2n, expectedPolicyRevision: 7n, runtimeConfigRevision: 5n, expectedServingRelease: "release-old", autoDeploy: true, idempotencyKey: expect.any(String) }) }));
    expect(createBuild).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText("源码快照 ID"), "-changed");
    expect(screen.queryByRole("button", { name: "确认创建构建" })).toBeNull();
  });
  it("重试构建发送新完整输入且失败重试保留幂等键", async () => {
    transport();
    vi.spyOn(platform.builds, "previewBuild").mockResolvedValue(create(PreviewBuildResponseSchema, { preview: { sourceSnapshotId: "src1", execution: { policyRevision: 7n, ready: true } } }));
    const retry = vi.spyOn(platform.builds, "retryBuild").mockRejectedValue(new Error("网络中断"));
    mount("/w/w1/apps/a1/builds/new?retryOf=b-old&snapshotId=src1"); const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "预览构建输入" }));
    await user.click(await screen.findByRole("button", { name: "确认重试构建" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: /^确认$/ }));
    await within(dialog).findByRole("alert");
    await user.click(within(dialog).getByRole("button", { name: /^确认$/ }));
    expect(retry).toHaveBeenCalledTimes(2);
    expect(retry.mock.calls[0][0]).toEqual(expect.objectContaining({ buildId: "b-old", input: expect.objectContaining({ sourceSnapshotId: "src1", expectedPolicyRevision: 7n, runtimeConfigRevision: 5n }) }));
    expect(retry.mock.calls[0][0].input?.idempotencyKey).toBe(retry.mock.calls[1][0].input?.idempotencyKey);
  });
  it("构建日志当纯文本展示并沿服务端序号增量读取", async () => {
    transport();
    const logs = vi.spyOn(platform.builds, "readBuildLogs").mockResolvedValue(create(ReadBuildLogsResponseSchema, { chunks: [{ seq: 12n, content: '<img src=x onerror="alert(1)">日志内容' }], nextSeq: 12n, complete: false }));
    mount("/w/w1/apps/a1/builds/b1"); const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "加载日志" }));
    expect(await screen.findByText('<img src=x onerror="alert(1)">日志内容')).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    await user.click(screen.getByRole("button", { name: "加载后续日志" }));
    expect(logs.mock.calls[1][0]).toEqual(expect.objectContaining({ workspaceId: "w1", applicationId: "a1", buildId: "b1", afterSeq: 12n }));
  });
  it("部署检查绑定最新草稿/当前服务/generation，确认前不发送部署", async () => {
    transport();
    vi.spyOn(platform.deployments, "getRuntimeStatus").mockResolvedValue(create(GetRuntimeStatusResponseSchema, { status: { generation: 4n, servingReleaseId: "serving-current" } }));
    vi.spyOn(platform.runtime, "getRuntimeConfig").mockResolvedValue(create(GetRuntimeConfigResponseSchema, { config: { revision: 9n, confirmed: true } }));
    vi.spyOn(platform.inventory, "resolveExecution").mockResolvedValue(create(ResolveExecutionResponseSchema, { summary: { policyRevision: 8n, ready: true } }));
    const deploy = vi.spyOn(platform.deployments, "createDeployment").mockRejectedValue(new Error("暂时断线"));
    mount("/w/w1/apps/a1/deployments?artifactId=artifact1"); const user = userEvent.setup();
    await waitFor(() => expect((screen.getByLabelText("运行配置版本（只读）") as HTMLInputElement).value).toBe("5"));
    await user.click(screen.getByRole("button", { name: "检查部署更改" }));
    // 页面最初读取配置 v5；检查后以最新 v9 拒绝陈旧表单，不能自动替换已检查版本。
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "请选择当前最新且已确认的运行配置；历史版本恢复请通过回滚操作。");
    expect(deploy).not.toHaveBeenCalled();
  });
  it("部署确认发送最新draft CAS和幂等键，不提前宣告健康", async () => {
    transport();
    vi.spyOn(platform.deployments, "getRuntimeStatus").mockResolvedValue(create(GetRuntimeStatusResponseSchema, { status: { generation: 4n, servingReleaseId: "serving-current" } }));
    vi.spyOn(platform.runtime, "getRuntimeConfig").mockResolvedValue(create(GetRuntimeConfigResponseSchema, { config: { revision: 5n, confirmed: true } }));
    vi.spyOn(platform.inventory, "resolveExecution").mockResolvedValue(create(ResolveExecutionResponseSchema, { summary: { policyRevision: 8n, ready: true } }));
    const deploy = vi.spyOn(platform.deployments, "createDeployment").mockRejectedValue(new Error("暂时断线"));
    mount("/w/w1/apps/a1/deployments?artifactId=artifact1"); const user = userEvent.setup();
    await waitFor(() => expect((screen.getByLabelText("运行配置版本（只读）") as HTMLInputElement).value).toBe("5"));
    await user.click(screen.getByRole("button", { name: "检查部署更改" }));
    await user.click(await screen.findByRole("button", { name: "确认部署此版本" }));
    expect(deploy).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: /^确认$/ }));
    await within(dialog).findByRole("alert");
    await user.click(within(dialog).getByRole("button", { name: /^确认$/ }));
    expect(deploy).toHaveBeenCalledTimes(2);
    expect(deploy.mock.calls[0][0]).toEqual(expect.objectContaining({ artifactId: "artifact1", runtimeConfigRevision: 5n, expectedPolicyRevision: 8n, expected: { generation: 4n, servingReleaseId: "serving-current", configRevision: 5n }, idempotencyKey: expect.any(String) }));
    expect(deploy.mock.calls[0][0].idempotencyKey).toBe(deploy.mock.calls[1][0].idempotencyKey);
    expect(screen.queryByText("应用已上线")).toBeNull();
  });
  it("无secret:write时不显示构建密钥管理或请求密钥列表", async () => {
    vi.spyOn(platform.transport, "unary").mockImplementation(async (method) => ({ stream: false, service: method.parent, method, header: new Headers(), trailer: new Headers(), message: create(method.output, method.name === "GetApplication" ? { application: { id: "a1", workspaceId: "w1", name: "app", capabilities: ["config:read"] } } : method.name === "GetWorkspace" ? { workspace: { id: "w1", slug: "w1" } } : { config: { dockerfile: "Dockerfile", context: "." } }) }));
    const secrets = vi.spyOn(platform.buildSecrets, "listBuildSecrets");
    mount("/w/w1/apps/a1/settings/build");
    await screen.findByRole("heading", { name: "构建设置" });
    expect(screen.queryByRole("button", { name: "创建构建密钥" })).toBeNull();
    expect(secrets).not.toHaveBeenCalled();
  });
});
