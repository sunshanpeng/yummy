import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { IdentityService } from "../gen/platform/v1/identity_pb";
import { OAuthAdminService } from "../gen/platform/v1/oauth_admin_pb";
import { ApplicationService, WorkspaceService } from "../gen/platform/v1/resources_pb";
import { RuntimeConfigService } from "../gen/platform/v1/runtime_config_pb";
import { SourceService } from "../gen/platform/v1/source_pb";
import { InventoryService } from "../gen/platform/v1/inventory_pb";
import { EnrollmentService } from "../gen/platform/v1/enrollment_pb";
import { BuildService } from "../gen/platform/v1/build_pb";
import { BuildSecretService } from "../gen/platform/v1/build_secret_pb";
import { DeploymentService } from "../gen/platform/v1/deployment_pb";

export type OAuthConnection = { id: string; clientId: string; resource: string; applicationIds: string[]; scopes: string[]; createdAt: string; expiresAt: string; revoked: boolean };

// CSRF 只放在内存；会话由 HttpOnly Cookie 保存，不向浏览器存储写入秘密。
export function createPlatformConnection(fetcher: typeof fetch = (...args) => fetch(...args)) {
  let csrfToken = "";
  let pending: Promise<string> | undefined;
  async function csrf(): Promise<string> {
    if (csrfToken) return csrfToken;
    pending ??= fetcher("/auth/csrf", { credentials: "same-origin", cache: "no-store" })
      .then(async (response) => {
        if (!response.ok) throw new Error("无法初始化安全会话，请重试");
        const data: unknown = await response.json();
        if (!data || typeof data !== "object" || !("csrfToken" in data) || typeof data.csrfToken !== "string" || !data.csrfToken) {
          throw new Error("服务端未返回 CSRF 令牌");
        }
        csrfToken = data.csrfToken;
        return csrfToken;
      }).finally(() => { pending = undefined; });
    return pending;
  }
  const security: Interceptor = (next) => async (request) => {
    request.header.set("X-CSRF-Token", await csrf());
    // 写请求不自动重放，避免令牌过期或网络错误触发重复操作。
    try { return await next(request); }
    catch (error) {
      if (error instanceof ConnectError && error.code === Code.Unauthenticated && typeof window !== "undefined") {
        window.dispatchEvent(new Event("yummy:session-expired"));
      }
      throw error;
    }
  };
  const transport = createConnectTransport({
    baseUrl: "/api", useHttpGet: false, interceptors: [security],
    fetch: (input, init) => fetcher(input, { ...init, credentials: "same-origin" }),
  });
  return {
    transport,
    identity: createClient(IdentityService, transport),
    oauthAdmin: createClient(OAuthAdminService, transport),
    workspaces: createClient(WorkspaceService, transport),
    applications: createClient(ApplicationService, transport),
    runtime: createClient(RuntimeConfigService, transport),
    sources: createClient(SourceService, transport),
    inventory: createClient(InventoryService, transport),
    enrollment: createClient(EnrollmentService, transport),
    builds: createClient(BuildService, transport),
    buildSecrets: createClient(BuildSecretService, transport),
    deployments: createClient(DeploymentService, transport),
    async listConnections(signal?: AbortSignal): Promise<{ connections: OAuthConnection[] }> {
      const response = await fetcher("/oauth/connections", { credentials: "same-origin", cache: "no-store", signal });
      if (!response.ok) throw new Error(response.status === 404 ? "实例未启用 OAuth 接入。" : "无法读取个人 Agent 连接。");
      return response.json() as Promise<{ connections: OAuthConnection[] }>;
    },
    async revokeConnection(id: string) {
      const response = await fetcher("/oauth/connections/revoke", { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json", "X-CSRF-Token": await csrf() }, body: JSON.stringify({ id }) });
      if (!response.ok) throw new Error("无法撤销连接，请刷新后重试。");
    },
    async uploadSource(path: string, body: File, signal?: AbortSignal) {
      if (!path.startsWith("/transfer/") || path.startsWith("//")) throw new Error("服务端返回的文件传输地址无效");
      const response = await fetcher(path, { method: "PUT", credentials: "same-origin", headers: { "X-CSRF-Token": await csrf(), "Content-Type": "application/gzip" }, body, signal });
      if (!response.ok) throw new Error(`源码传输未完成（HTTP ${response.status}），请确认会话和网络后重试。`);
    },
    setCsrfToken(value: string) { if (value) csrfToken = value; },
    resetCsrf() { csrfToken = ""; },
  };
}

export const platform = createPlatformConnection();
export function errorMessage(error: unknown): string {
  if (error instanceof ConnectError) {
    const detail = error.rawMessage;
    if (error.code === Code.Unauthenticated) return "会话已过期，请重新登录。";
    if (error.code === Code.PermissionDenied) return `没有执行此操作的权限。${detail}`;
    if (error.code === Code.NotFound) return "资源不存在、已归档，或当前账户无权访问。";
    if (error.code === Code.Aborted || error.code === Code.AlreadyExists) return `数据发生冲突，请刷新后重试。${detail}`;
    if (error.code === Code.Unavailable) return "暂时无法连接控制面，请检查网络后重试。";
    return detail || "请求未完成，请重试。";
  }
  return error instanceof Error ? error.message : "请求未完成，请重试。";
}

export const tokenScopes = ["application:read", "source:read", "source:upload", "build:read", "build:write", "logs:read", "deployment:read", "deployment:write", "runtime:read", "runtime:write", "config:read", "config:write"];
export const defaultTokenScopes = ["application:read", "build:read", "logs:read", "deployment:read", "runtime:read", "config:read"];
export const can = (resource: { capabilities: string[] } | undefined, capability: string) => resource?.capabilities.includes(capability) ?? false;
export const formatTime = (value: string) => value && Number.isFinite(Date.parse(value)) ? new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(value)) : "—";
export const roleLabel = (value: string) => ({ admin: "空间管理员", member: "普通成员", instance_admin: "全局管理员", workspace_admin: "空间管理员", viewer: "查看者", developer: "开发者", maintainer: "应用管理员" }[value] ?? value);
