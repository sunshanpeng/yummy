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
import { GetApplicationResponseSchema, GetWorkspaceResponseSchema, ListWorkspacesResponseSchema } from "../gen/platform/v1/resources_pb";
import { ListDeploymentsResponseSchema, GetRuntimeLogsResponseSchema, GetRuntimeMetricsResponseSchema, GetRuntimeStatusResponseSchema, RuntimeLogChunkSchema, type RuntimeLogChunk, type RuntimeMetricSample } from "../gen/platform/v1/deployment_pb";

const encoder = new TextEncoder();
type Message = Record<string, unknown>;
type Handler = (message: Message) => unknown;

class ResizeObserverStub {
  private callback: ResizeObserverCallback;
  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
  }
  observe(target: Element) {
    this.callback([{ target, contentRect: { width: 640, height: 240, x: 0, y: 0, top: 0, left: 0, right: 640, bottom: 240 } } as ResizeObserverEntry], this as unknown as ResizeObserver);
  }
  unobserve() {}
  disconnect() {}
}

function makeChunk(id: string, text: Uint8Array, createdAt: string, overrides: Partial<Message> = {}): RuntimeLogChunk {
  return create(RuntimeLogChunkSchema, { id, workspaceId: "w1", applicationId: "a1", podUid: "pod-1", container: "app", restartCount: 0, seq: BigInt(id.length), content: text, truncated: false, createdAt, ...overrides });
}

function makeSample(id: string, collectedAt: string, cpuNanoCores: bigint, cpuWindowMillis: bigint, memoryBytes: bigint): RuntimeMetricSample {
  return { id, workspaceId: "w1", applicationId: "a1", releaseId: "rel-1", generation: 1n, podUid: "pod-1", cpuNanoCores, cpuWindowMillis, memoryWorkingSetBytes: memoryBytes, collectedAt, $typeName: "platform.v1.RuntimeMetricSample" };
}

// 挂载应用部署页；handlers 按方法名拦截 Connect 传输层，未处理的直接失败，避免测试悄悄拿到假数据。
function mount(handlers: Record<string, Handler>) {
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
  client.setQueryData(sessionOptions.queryKey, create(GetSessionResponseSchema, { csrfToken: "csrf", user: { id: "user-current", username: "current", status: "ACTIVE", instanceAdmin: false, authVersion: 1n } }));
  const instance = createRouter({ routeTree: router.options.routeTree, history: createMemoryHistory({ initialEntries: ["/w/w1/apps/a1/deployments"] }) });
  render(<QueryClientProvider client={client}><RouterProvider router={instance} /></QueryClientProvider>);
  return calls;
}

function baseHandlers(observedState: string): Record<string, Handler> {
  return {
    ListWorkspaces: () => create(ListWorkspacesResponseSchema, { workspaces: [] }),
    GetWorkspace: () => create(GetWorkspaceResponseSchema, { workspace: { id: "w1", name: "空间", capabilities: ["workspace:read"] } }),
    GetApplication: () => create(GetApplicationResponseSchema, { application: { id: "a1", workspaceId: "w1", name: "应用", status: "ACTIVE", revision: 3n, capabilities: ["application:read", "deployment:read", "runtime:read"] } }),
    ListDeployments: () => create(ListDeploymentsResponseSchema, { deployments: [] }),
    GetRuntimeStatus: () => create(GetRuntimeStatusResponseSchema, { status: { workspaceId: "w1", applicationId: "a1", generation: 2n, desiredState: "RUN", observedState, servingReleaseId: "rel-1", observedGeneration: 2n, lastObservedAt: "2026-11-10T10:00:00Z" } }),
  };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("应用运行日志与基础监控", () => {
  it("应用未部署时展示明确空态且不请求日志或指标", () => {
    const calls = mount({ ...baseHandlers("NOT_DEPLOYED") });
    return screen.findByText("应用当前状态为「尚未部署」，没有正在运行的实例，因此没有运行日志或监控采样。部署成功并启动后，这里展示 worker 上报的真实数据。").then(() => {
      expect(calls.GetRuntimeLogs).toBeUndefined();
      expect(calls.GetRuntimeMetrics).toBeUndefined();
    });
  });

  it("已停止应用同样不拉取日志与指标", () => {
    const calls = mount({ ...baseHandlers("STOPPED") });
    return screen.findByText(/没有正在运行的实例/).then(() => {
      expect(calls.GetRuntimeLogs).toBeUndefined();
      expect(calls.GetRuntimeMetrics).toBeUndefined();
    });
  });

  it("日志读取失败显示可重试错误并成功恢复", async () => {
    const user = userEvent.setup();
    let failing = true;
    const calls = mount({
      ...baseHandlers("RUNNING"),
      GetRuntimeLogs: () => { if (failing) throw new ConnectError("control plane unavailable", Code.Unavailable); return create(GetRuntimeLogsResponseSchema, { chunks: [makeChunk("log-1", encoder.encode("恢复后的日志"), "2026-11-10T10:00:01Z")], nextCursor: "" }); },
      GetRuntimeMetrics: () => create(GetRuntimeMetricsResponseSchema, { samples: [] }),
    });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("暂时无法连接控制面");
    expect(screen.getByRole("button", { name: "重新加载" })).toBeTruthy();
    failing = false;
    await user.click(screen.getByRole("button", { name: "重新加载" }));
    expect(await screen.findByText("恢复后的日志")).toBeTruthy();
    expect(calls.GetRuntimeLogs.length).toBeGreaterThan(1);
  });

  it("日志按序拼接、跨块 UTF-8 正确解码、空块不渲染，并按游标加载更新", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("ResizeObserver", ResizeObserverStub);
    const hello = encoder.encode("你好");
    const calls = mount({
      ...baseHandlers("RUNNING"),
      GetRuntimeLogs: (message) => {
        if (!message.cursor) {
          // “你”占 3 字节，“好”占 3 字节：第一块只带 4 字节，跨块序列由下一块续上。
          return create(GetRuntimeLogsResponseSchema, {
            chunks: [
              makeChunk("log-a", hello.subarray(0, 4), "2026-11-10T10:00:01Z"),
              makeChunk("log-b", new Uint8Array([...hello.subarray(4), ...encoder.encode(" 第二块")]), "2026-11-10T10:00:02Z"),
            ],
            nextCursor: "cursor-1",
          });
        }
        return create(GetRuntimeLogsResponseSchema, {
          chunks: [makeChunk("log-empty", new Uint8Array(0), "2026-11-10T10:00:03Z"), makeChunk("log-c", encoder.encode("更新的日志块"), "2026-11-10T10:00:04Z", { truncated: true })],
          nextCursor: "",
        });
      },
      GetRuntimeMetrics: () => create(GetRuntimeMetricsResponseSchema, { samples: [] }),
    });
    expect(await screen.findByText("你")).toBeTruthy();
    expect(screen.getByText("好 第二块")).toBeTruthy();
    expect(screen.getByText("已显示 2 个日志块（按时间正序），服务端还有更多。")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "加载更新的日志" }));
    expect(await screen.findByText("更新的日志块")).toBeTruthy();
    expect(screen.getByText("该块超长已被截断")).toBeTruthy();
    expect(screen.getByText("已显示 3 个日志块（按时间正序），已到保留窗口最新。")).toBeTruthy();
    // 空内容块不应渲染。
    expect(screen.queryByText((_, element) => element?.textContent === "" && element.tagName === "P")).toBeNull();
    // 第二次分页必须携带服务端返回的键集游标。
    expect(calls.GetRuntimeLogs.some((message) => message.cursor === "cursor-1" && message.limit === 50)).toBe(true);
    // 滚动到底部按钮可点击且不抛错（jsdom 无 Element.scrollTo，实现需有回退）。
    await user.click(screen.getByRole("button", { name: "滚动到日志底部" }));
  });

  it("多采样点渲染 CPU/内存图表，窗口切换重新请求对应 since", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("ResizeObserver", ResizeObserverStub);
    const samples = [
      makeSample("s-1", "2026-11-10T09:00:10Z", 500_000_000n, 1000n, 512n * 1024n * 1024n),
      makeSample("s-2", "2026-11-10T09:00:40Z", 250_000_000n, 1000n, 600n * 1024n * 1024n),
      makeSample("s-3", "2026-11-10T09:01:10Z", 1_200_000_000n, 1000n, 700n * 1024n * 1024n),
    ];
    const calls = mount({
      ...baseHandlers("RUNNING"),
      GetRuntimeLogs: () => create(GetRuntimeLogsResponseSchema, { chunks: [] }),
      GetRuntimeMetrics: (message) => {
        expect(message.until).toBe("");
        return create(GetRuntimeMetricsResponseSchema, { samples });
      },
    });
    await screen.findByText("基础监控");
    await vi.waitFor(() => expect(document.querySelector(".recharts-surface")).toBeTruthy());
    expect(screen.getByText("CPU")).toBeTruthy();
    expect(screen.getByText("内存")).toBeTruthy();
    const metricCalls = calls.GetRuntimeMetrics;
    await user.click(screen.getByRole("tab", { name: "6 小时" }));
    await vi.waitFor(() => expect(metricCalls.length).toBeGreaterThan(1));
    const since = Date.parse(String(metricCalls.at(-1)?.since));
    expect(since).toBeLessThanOrEqual(Date.now() - 350 * 60 * 1000);
  });

  it("单采样点降级为真实读数摘要而不绘制趋势图", async () => {
    vi.stubGlobal("ResizeObserver", ResizeObserverStub);
    mount({
      ...baseHandlers("RUNNING"),
      GetRuntimeLogs: () => create(GetRuntimeLogsResponseSchema, { chunks: [] }),
      GetRuntimeMetrics: () => create(GetRuntimeMetricsResponseSchema, { samples: [makeSample("s-1", "2026-11-10T09:00:10Z", 812_000_000n, 1000n, 128n * 1024n * 1024n)] }),
    });
    const summary = await screen.findByText(/窗口内只有 1 个采样点/);
    expect(summary.textContent).toContain("812 毫核");
    expect(summary.textContent).toContain("128 MiB");
    expect(document.querySelector(".recharts-surface")).toBeNull();
  });
});
