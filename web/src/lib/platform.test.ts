import { create, toJson } from "@bufbuild/protobuf";
import { afterEach, describe, expect, it, vi } from "vitest";
import { GetSessionResponseSchema } from "../gen/platform/v1/identity_pb";
import { createPlatformConnection, can } from "./platform";

const jsonResponse = (schema: Parameters<typeof create>[0], value: Record<string, unknown>) => new Response(JSON.stringify(toJson(schema, create(schema, value))), { status: 200, headers: { "Content-Type": "application/json" } });
afterEach(() => vi.restoreAllMocks());
describe("真实 Connect 安全传输", () => {
  it("先 GET CSRF，再以 Cookie 与 X-CSRF-Token POST GetSession；匿名 user 可为空", async () => {
    const requests: { url: string; init?: RequestInit }[] = [];
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ url: String(input), init });
      if (String(input) === "/auth/csrf") return new Response(JSON.stringify({ csrfToken: "csrf-one" }), { headers: { "Content-Type": "application/json" } });
      return jsonResponse(GetSessionResponseSchema, { csrfToken: "csrf-one" });
    });
    const connection = createPlatformConnection(fetcher);
    const session = await connection.identity.getSession({});
    expect(session.user).toBeUndefined();
    expect(requests[0].url).toBe("/auth/csrf");
    expect(requests[0].init?.credentials).toBe("same-origin");
    expect(requests[1].url).toContain("/api/platform.v1.IdentityService/GetSession");
    expect(requests[1].init?.method).toBe("POST");
    expect(requests[1].init?.credentials).toBe("same-origin");
    expect(new Headers(requests[1].init?.headers).get("X-CSRF-Token")).toBe("csrf-one");
  });
  it("并发请求共享 bootstrap，失败写操作不自动重放", async () => {
    const fetcher = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input) === "/auth/csrf") return new Response('{"csrfToken":"csrf-token"}', { headers: { "Content-Type": "application/json" } });
      if (String(input).includes("Login")) return new Response('{"code":"permission_denied","message":"拒绝"}', { status: 403, headers: { "Content-Type": "application/json" } });
      return jsonResponse(GetSessionResponseSchema, {});
    });
    const connection = createPlatformConnection(fetcher);
    await Promise.all([connection.identity.getSession({}), connection.identity.getSession({})]);
    await expect(connection.identity.login({ username: "root", password: "invalid" })).rejects.toThrow();
    expect(fetcher.mock.calls.filter(([url]) => String(url) === "/auth/csrf")).toHaveLength(1);
    expect(fetcher.mock.calls.filter(([url]) => String(url).includes("Login"))).toHaveLength(1);
  });
  it("源码PUT仍携带Cookie与CSRF且拒绝外部传输地址", async () => {
    const fetcher = vi.fn(async (input: RequestInfo | URL) => String(input) === "/auth/csrf" ? new Response('{"csrfToken":"csrf-upload"}', { headers: { "Content-Type": "application/json" } }) : new Response(null, { status: 204 }));
    const connection = createPlatformConnection(fetcher);
    const file = new File(["archive"], "source.tar.gz");
    await connection.uploadSource("/transfer/source/upload-1", file);
    expect(fetcher).toHaveBeenLastCalledWith("/transfer/source/upload-1", expect.objectContaining({ method: "PUT", body: file, credentials: "same-origin", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-upload" }) }));
    await expect(connection.uploadSource("https://external.example/transfer/secret", file)).rejects.toThrow("地址无效");
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
  it("CSRF bootstrap无效时不发送任何 Connect 写请求", async () => {
    const fetcher = vi.fn(async () => new Response("{}", { headers: { "Content-Type": "application/json" } }));
    await expect(createPlatformConnection(fetcher).identity.login({ username: "test", password: "bad" })).rejects.toThrow("CSRF");
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
describe("能力只读取后端结果", () => {
  it("不根据角色或管理员布尔值推导业务权限", () => {
    expect(can({ capabilities: ["application:read"] }, "config:write")).toBe(false);
    expect(can({ capabilities: ["member:write"] }, "member:write")).toBe(true);
    expect(can(undefined, "application:create")).toBe(false);
  });
});
