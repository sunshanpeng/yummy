import { create } from "@bufbuild/protobuf";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { GetSessionResponseSchema } from "../gen/platform/v1/identity_pb";
import { platform } from "../lib/platform";
import { router } from "../router";
import { sessionOptions } from "../session";

function mount(path: string) {
  vi.spyOn(window, "scrollTo").mockImplementation(() => {});
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(sessionOptions.queryKey, create(GetSessionResponseSchema, { csrfToken: "csrf", user: { id: "user-1", username: "dev", status: "ACTIVE", authVersion: 1n } }));
  const instance = createRouter({ routeTree: router.options.routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  render(<QueryClientProvider client={client}><RouterProvider router={instance} /></QueryClientProvider>);
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
it("授权页明确展示客户端和权限，未选择应用不发送允许请求", async () => {
  const fetcher = vi.spyOn(window, "fetch").mockResolvedValue(new Response(JSON.stringify({ requestId: "req_abcdefghijklmnopqrstuv", csrfToken: "grant-csrf", formCsrfToken: "session-csrf", clientName: "Agent A", clientId: "client-a", clientSource: "pre-registered", resource: "https://example.test/api", scopes: ["application:read", "offline_access"], applications: [], availableApplications: [{ id: "app_1", workspaceId: "ws_1", name: "gateway" }], offlineAccess: true }), { status: 200, headers: { "Content-Type": "application/json" } }));
  mount("/authorize?request_id=req_abcdefghijklmnopqrstuv");
  expect(await screen.findByText(/Agent A/)).toBeTruthy();
  expect(screen.getByText("application:read")).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "允许授权" }));
  expect(await screen.findByRole("alert")).toHaveProperty("textContent", "请至少选择一个应用，或拒绝本次授权。");
  expect(fetcher).toHaveBeenCalledTimes(1);
});
it("普通用户可进入自己 Agent 连接页面，撤销需要确认", async () => {
  vi.spyOn(platform, "listConnections").mockResolvedValue({ connections: [{ id: "grt_1", clientId: "agent-a", resource: "https://example.test/api", applicationIds: ["app_1"], scopes: ["application:read"], createdAt: "2026-01-01T00:00:00Z", expiresAt: "2026-12-01T00:00:00Z", revoked: false }] });
  const revoke = vi.spyOn(platform, "revokeConnection").mockResolvedValue();
  mount("/account/connections");
  expect(await screen.findByText("agent-a")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "切换工作空间" })).toBeNull();
  await userEvent.click(screen.getByRole("button", { name: "撤销连接" }));
  expect(revoke).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole("button", { name: "确认" }));
  expect(revoke).toHaveBeenCalledWith("grt_1");
});
