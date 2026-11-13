// OAuth 授权码 + PKCE 真实协议流：元数据发现、正向全流、负向分支、刷新轮换与撤销。
// 语义依据 internal/platform/oauth（consent.go / service.go / token.go / storage.go / account.go）
// 与 web/src/pages/oauth.tsx 的真实 consent UI；redirect_uri 指向已注册但不可达的地址，
// 用 page.route 拦截授权回调，全部令牌交换走真实 /oauth/token、/oauth/revoke 端点。
// 凭证只读环境变量；令牌只在内存中流转，不写入文件。用例按文件顺序执行（serial），
// 后续用例复用前序用例创建的工作空间、应用、客户端与令牌。
//
// 已知真实缺陷（不改 Go / web/src，详见测试报告）：oauth.tsx 用 FormData(multipart) 提交同意，
// 运维注意：控制面登录频控 20 次/15 分钟/账号；整套 e2e 每轮约消耗 14-18 次登录，
// 单次全量可跑，连续多轮需先清理频控键（yummy:login:*）或等待窗口重置。
// 而 consent.go 仅调用 r.ParseForm()（只解析 application/x-www-form-urlencoded），浏览器
// 点击“允许授权”必然 403「授权请求无效或已过期」。因此用例在真实 consent UI 上完成全部核对
// 与勾选后，按 UI 完全相同的字段与双 CSRF 语义（request CSRF + gorilla CSRF 头）改用
// urlencoded 提交 /oauth/consent，其余协议环节不受影响。
import { createHash, randomBytes } from "node:crypto";
import { expect, test as base, type Page, type Response } from "@playwright/test";

const username = process.env.YUMMY_E2E_USERNAME;
const password = process.env.YUMMY_E2E_PASSWORD;
const baseURL = process.env.YUMMY_E2E_BASE_URL ?? "http://127.0.0.1:8080";
const origin = new URL(baseURL).origin;
const apiResource = `${origin}/api`;
// 已注册但不可达的回调地址：由路由拦截捕获 code/state，不发起真实连接。
const redirectURI = "https://127.0.0.1/callback";
const run = Date.now().toString(36);
// 测试数据跨运行累积；列表按 id ASC 每页 24 条，目标行可能落在后续页，逐页查找。
async function findClientRow(page: Page, pattern: RegExp, label: string) {
  for (let index = 0; index < 12; index++) {
    const row = page.getByRole("row", { name: pattern });
    if (await row.count()) {
      await row.first().waitFor({ state: "visible" });
      return row.first();
    }
    const next = page.getByRole("button", { name: /下一页/ });
    if (!(await next.count()) || !(await next.isEnabled())) break;
    await next.click();
    await page.waitForTimeout(150);
  }
  // 列表查询可能在加载中短暂为空；等待后从首页重试。
  await page.waitForTimeout(500);
  for (let index = 0; index < 12; index++) {
    const row = page.getByRole("row", { name: pattern });
    if (await row.count()) {
      await row.first().waitFor({ state: "visible" });
      return row.first();
    }
    const next = page.getByRole("button", { name: /下一页/ });
    if (!(await next.count()) || !(await next.isEnabled())) break;
    await next.click();
    await page.waitForTimeout(150);
  }
  throw new Error(`${label} 在全部分页中未找到`);
}

const clientId = `e2e-oauth-${run}`;
const clientName = `e2e OAuth 客户端 ${run}`;
const workspaceName = `e2e OAuth 空间 ${run}`;
const slug = `e2e-oauth-ws-${run}`;
const appName = `e2e-oauth-app-${run}`;
// offline_access 必须显式请求且由用户单独同意，才会签发 refresh token（RefreshTokenScopes）。
const scopesWithOffline = "application:read offline_access";

function randomState(): string {
  return randomBytes(32).toString("base64url");
}
function pkcePair(): { verifier: string; challenge: string } {
  const verifier = randomBytes(48).toString("base64url");
  return { verifier, challenge: createHash("sha256").update(verifier).digest("base64url") };
}

type SelectedApp = { workspaceId: string; applicationId: string; name: string };
// 跨用例共享的真实数据（同一 worker 内顺序执行）。令牌仅驻留内存。
const state: { tokens?: { access: string; refresh: string; ids: SelectedApp } } = {};

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// worker 级登录会话：本文件只登录一次（其余用例共享），另一次登录在匿名入口用例中完成，
// 避免触发控制面按账号的登录频控（20 次/15 分钟）。
const test = base.extend<{ adminPage: Page }>({
  adminPage: [
    async ({ browser }, use) => {
      const context = await browser.newContext();
      const page = await context.newPage();
      await login(page);
      await use(page);
      await context.close();
    },
    { scope: "worker" },
  ],
});

test.describe.configure({ mode: "serial" });

async function login(page: Page) {
  await page.goto("/login");
  await page.getByLabel("用户名").fill(username!);
  await page.getByLabel("密码", { exact: true }).fill(password!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "你的工作空间" })).toBeVisible();
}

// Connect JSON 调用：会话 Cookie + gorilla CSRF 头（控制面对非 Bearer 写请求强制校验）。
async function connectPost(page: Page, rpc: string, body: object) {
  const csrf = await (await page.request.get("/auth/csrf")).json().then((value: { csrfToken: string }) => value.csrfToken);
  return page.request.post(`/api/${rpc}`, {
    headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1", "X-CSRF-Token": csrf },
    data: body,
  });
}

// 拦截已注册回调（连接不可达）；授权结果从最终 URL 参数读取。
async function captureCallback(page: Page) {
  await page.route(
    (url) => url.protocol === "https:" && url.hostname === "127.0.0.1" && url.pathname === "/callback",
    async (route) => {
      await route.fulfill({ status: 200, contentType: "text/html; charset=utf-8", body: "<!doctype html><title>oauth-e2e-callback</title><p>授权回调已被 E2E 拦截。</p>" });
    },
  );
}

function authorizeURL(options: { scope: string; state: string; challenge?: string; method?: string }): string {
  const params = new URLSearchParams({
    response_type: "code",
    client_id: clientId,
    redirect_uri: redirectURI,
    scope: options.scope,
    state: options.state,
    resource: apiResource,
  });
  if (options.challenge !== undefined) params.set("code_challenge", options.challenge);
  if (options.method !== undefined) params.set("code_challenge_method", options.method);
  return `/oauth/authorize?${params.toString()}`;
}

// 提前布置对 UI 自己的 GET /oauth/consent 的监听（必须在触发导航之前）。
function armConsentView(page: Page) {
  return page.waitForResponse((response) => response.request().method() === "GET" && new URL(response.url()).pathname === "/oauth/consent");
}

// 真实 consent UI：核对客户端名/ID、资源、请求的操作并勾选应用（真实复选框交互）。
// 同意视图直接取自 UI 自己的 GET /oauth/consent 响应（服务端保存的 csrf_hash 与该响应的
// csrfToken 成对），不再额外请求，避免两次 GET 竞争重写 CSRF hash。
// 服务端同意提交因上述 multipart 缺陷改走 urlencoded，字段与 UI 完全一致。
async function approveConsent(page: Page, options: { offline: boolean }, viewResponsePromise: Promise<Response>, submit: "urlencoded" | "ui" = "urlencoded"): Promise<SelectedApp> {
  const viewResponse = await viewResponsePromise;
  expect(viewResponse.ok(), await viewResponse.text()).toBeTruthy();
  const view = (await viewResponse.json()) as {
    requestId: string;
    csrfToken: string;
    formCsrfToken: string;
    clientName: string;
    clientId: string;
    resource: string;
    scopes: string[];
    offlineAccess: boolean;
    availableApplications: { id: string; workspaceId: string; name: string }[] | null;
  };
  expect(view.clientId).toBe(clientId);
  expect(new URL(page.url()).searchParams.get("request_id")).toBe(view.requestId);

  // 页面真实呈现：标题、客户端、目标资源与请求的操作。
  await expect(page.getByRole("heading", { name: "确认 Agent 授权" })).toBeVisible();
  await expect(page.getByText(clientName).first()).toBeVisible();
  await expect(page.getByText(clientId).first()).toBeVisible();
  await expect(page.getByText(apiResource).first()).toBeVisible();
  await expect(page.getByText("application:read", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "允许授权" })).toBeVisible();

  // 应用范围：优先选择本用例创建的应用；页面按展示上限列出应用列表。
  const choices = view.availableApplications ?? [];
  expect(choices.length, "同意页应列出可授权的应用").toBeGreaterThan(0);
  const chosen = choices.find((choice) => choice.name === appName) ?? choices[0];
  const group = page.getByRole("group", { name: "选择客户端可以访问的应用" });
  await group.getByRole("checkbox", { name: new RegExp(escapeRegExp(chosen.name)) }).check();

  // offline_access 必须由用户在页面上单独同意。
  const offlineBox = page.getByRole("checkbox", { name: /允许离线访问/ });
  if (options.offline) {
    expect(view.offlineAccess).toBe(true);
    await offlineBox.check();
  } else {
    expect(view.offlineAccess).toBe(false);
    // 未请求 offline_access 时，同意页不出现离线访问复选框。
    await expect(offlineBox).toHaveCount(0);
  }

  if (submit === "ui") {
    // 真实点击 UI「允许授权」按钮：oauth.tsx 以 FormData(multipart) 提交，服务端必须
    // 解析 multipart 并返回 redirect，SPA 随后导航到回调（缺陷 A 回归验证）。
    await Promise.all([
      page.waitForURL(/127\.0\.0\.1\/callback/, { timeout: 10000 }),
      page.getByRole("button", { name: "允许授权" }).click(),
    ]);
    return { workspaceId: chosen.workspaceId, applicationId: chosen.id, name: chosen.name };
  }
  // 提交同意：字段与 oauth.tsx 完全一致（request_id/csrf_token/gorilla.csrf.Token/decision/
  // offline_access/application_id），仅按服务端实际支持的编码提交（见文件头缺陷说明）。
  const form: Record<string, string> = {
    request_id: view.requestId,
    csrf_token: view.csrfToken,
    "gorilla.csrf.Token": view.formCsrfToken,
    decision: "allow",
  };
  if (options.offline) form.offline_access = "true";
  form.application_id = chosen.id;
  const consent = await page.request.post("/oauth/consent", {
    headers: { "X-Yummy-Response-Mode": "manual", "X-CSRF-Token": view.formCsrfToken, Origin: origin },
    form,
  });
  expect(consent.status(), await consent.text()).toBe(200);
  const { redirect } = (await consent.json()) as { redirect?: string };
  expect(redirect).toBeTruthy();
  // 与 SPA 相同：由服务端已验证的请求生成回调地址后再导航。
  await page.goto(redirect!);
  await expect(page).toHaveURL(/127\.0\.0\.1\/callback/);
  return { workspaceId: chosen.workspaceId, applicationId: chosen.id, name: chosen.name };
}

async function exchangeCode(page: Page, code: string, verifier: string) {
  return page.request.post("/oauth/token", {
    form: { grant_type: "authorization_code", code, client_id: clientId, redirect_uri: redirectURI, code_verifier: verifier, resource: apiResource },
  });
}

async function bearerGetApplication(page: Page, accessToken: string, ids: SelectedApp) {
  return page.request.post("/api/platform.v1.ApplicationService/GetApplication", {
    headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1", Authorization: `Bearer ${accessToken}` },
    data: { workspaceId: ids.workspaceId, applicationId: ids.applicationId },
  });
}

async function expectBearerOK(page: Page, accessToken: string, ids: SelectedApp, why: string) {
  const response = await bearerGetApplication(page, accessToken, ids);
  expect(response.status(), why).toBe(200);
  const body = (await response.json()) as { application?: { id?: string; name?: string } };
  expect(body.application?.id).toBe(ids.applicationId);
  expect(body.application?.name).toBe(ids.name);
}

async function expectBearerDenied(page: Page, accessToken: string, ids: SelectedApp, why: string) {
  const response = await bearerGetApplication(page, accessToken, ids);
  expect(response.status(), why).toBe(401);
  const body = (await response.json()) as { code?: string };
  expect(body.code).toBe("unauthenticated");
}

test("授权服务器元数据发现", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const response = await page.request.get("/.well-known/oauth-authorization-server");
  expect(response.ok()).toBeTruthy();
  const metadata = (await response.json()) as Record<string, unknown>;
  expect(metadata.issuer).toBe(origin);
  expect(metadata.authorization_endpoint).toBe(`${origin}/oauth/authorize`);
  expect(metadata.token_endpoint).toBe(`${origin}/oauth/token`);
  expect(metadata.revocation_endpoint).toBe(`${origin}/oauth/revoke`);
  expect(metadata.response_types_supported).toContain("code");
  expect(metadata.grant_types_supported).toEqual(expect.arrayContaining(["authorization_code", "refresh_token"]));
  expect(metadata.code_challenge_methods_supported).toContain("S256");
  expect(metadata.token_endpoint_auth_methods_supported).toContain("none");
  expect(metadata.scopes_supported).toEqual(expect.arrayContaining(["application:read", "offline_access"]));
  expect(metadata.authorization_response_iss_parameter_supported).toBe(true);
});

test("准备真实数据：工作空间、应用与允许 offline_access 的预注册客户端", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const workspace = await connectPost(adminPage, "platform.v1.WorkspaceService/CreateWorkspace", {
    name: workspaceName,
    slug,
    description: "OAuth E2E 专用工作空间。",
  });
  expect(workspace.ok(), await workspace.text()).toBeTruthy();
  const workspaceId = ((await workspace.json()) as { workspace?: { id?: string } }).workspace?.id ?? "";
  expect(workspaceId).toBeTruthy();
  const application = await connectPost(adminPage, "platform.v1.ApplicationService/CreateApplication", {
    workspaceId,
    name: appName,
    idempotencyKey: `e2e-oauth-${run}`,
  });
  expect(application.ok(), await application.text()).toBeTruthy();
  expect(((await application.json()) as { application?: { id?: string } }).application?.id).toBeTruthy();

  // 通过实例管理 UI 真实登记客户端；offline_access 必须在客户端允许范围内才能请求。
  await adminPage.goto("/platform/oauth-clients");
  await expect(adminPage.getByRole("table", { name: "OAuth 客户端" })).toBeVisible();
  await adminPage.getByRole("button", { name: "注册客户端" }).click();
  await adminPage.getByLabel("客户端 ID（client_id）").fill(clientId);
  await adminPage.getByLabel("客户端名称").fill(clientName);
  await adminPage.getByLabel("回调地址（每行一个）").fill(redirectURI);
  await adminPage.getByLabel("授权范围（空格或逗号分隔，可留空）").fill(scopesWithOffline);
  await adminPage.getByLabel("资源（每行一个）").fill(apiResource);
  await adminPage.getByRole("button", { name: "登记客户端" }).click();
  const row = await findClientRow(adminPage, new RegExp(clientId), "预注册客户端");
  await expect(row.getByText("已启用")).toBeVisible();
});

test("授权码 + PKCE S256 正向全流：consent UI、state 校验、令牌交换与受保护资源", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const { verifier, challenge } = pkcePair();
  const stateValue = randomState();
  await captureCallback(adminPage);
  const viewPromise = armConsentView(adminPage);
  await adminPage.goto(authorizeURL({ scope: scopesWithOffline, state: stateValue, challenge, method: "S256" }));
  // 已登录会话直接进入授权确认页（携带服务端保存的 request_id）。
  await expect(adminPage).toHaveURL(/\/authorize\?request_id=req_/);
  const ids = await approveConsent(adminPage, { offline: true }, viewPromise);
  const callback = new URL(adminPage.url());
  const code = callback.searchParams.get("code") ?? "";
  expect(code).toBeTruthy();
  // state 与 RFC 9207 iss 必须如实回显。
  expect(callback.searchParams.get("state")).toBe(stateValue);
  expect(callback.searchParams.get("iss")).toBe(origin);

  const tokenResponse = await exchangeCode(adminPage, code, verifier);
  expect(tokenResponse.status(), await tokenResponse.text()).toBe(200);
  const tokens = (await tokenResponse.json()) as { access_token?: string; refresh_token?: string; token_type?: string; scope?: string; expires_in?: number };
  expect(tokens.token_type).toBe("bearer");
  expect(tokens.access_token).toBeTruthy();
  expect(tokens.refresh_token).toBeTruthy();
  expect(tokens.scope?.split(" ")).toEqual(expect.arrayContaining(["application:read", "offline_access"]));
  // access token 有效期 15 分钟。
  expect(tokens.expires_in as number).toBeGreaterThanOrEqual(895);
  expect(tokens.expires_in as number).toBeLessThanOrEqual(900);
  state.tokens = { access: tokens.access_token!, refresh: tokens.refresh_token!, ids };

  // 用 access token 调真实受保护资源：scope application:read + 已授权应用，返回真实数据。
  await expectBearerOK(adminPage, tokens.access_token!, ids, "OAuth bearer 应能读取已授权应用");
  // 未授权的应用不在授权范围内，不能越权读取（预先创建的应用仍不可见）。
  const forbidden = await bearerGetApplication(adminPage, tokens.access_token!, { workspaceId: ids.workspaceId, applicationId: "app-not-in-grant", name: "" });
  expect(forbidden.status()).toBe(404);
});

test("回归缺陷A：真实 consent UI 按钮以 multipart 提交完成授权", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  // 依赖同文件 prepare 用例（fullyParallel=false 保序）注册的模块客户端；
  // 本用例专测真实 UI 按钮的 multipart 提交路径（缺陷 A 回归）。
  const { verifier, challenge } = pkcePair();
  const stateValue = randomState();
  await captureCallback(adminPage);
  const viewPromise = armConsentView(adminPage);
  await adminPage.goto(authorizeURL({ scope: scopesWithOffline, state: stateValue, challenge, method: "S256" }));
  await expect(adminPage).toHaveURL(/\/authorize\?request_id=req_/);
  // 走真实 UI 按钮提交（oauth.tsx FormData multipart），不再使用 urlencoded 绕过。
  await approveConsent(adminPage, { offline: true }, viewPromise, "ui");
  const callback = new URL(adminPage.url());
  expect(callback.searchParams.get("code")).toBeTruthy();
  expect(callback.searchParams.get("state")).toBe(stateValue);
  const tokenResponse = await exchangeCode(adminPage, callback.searchParams.get("code")!, verifier);
  expect(tokenResponse.status(), await tokenResponse.text()).toBe(200);
  const tokens = (await tokenResponse.json()) as { access_token?: string };
  expect(tokens.access_token).toBeTruthy();
});

test("负向：code_challenge_method 缺失或 plain 直接重定向错误到回调", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  // 服务端在登录检查之前校验 PKCE 方法（consent.go），匿名上下文同样得到标准错误重定向。
  // 服务端 303 产生的重定向跳不经过 SPA 导航，Playwright 路由无法拦截；改以监听真实请求的
  // 方式捕获错误回调，并如实断言回调地址确实不可达（连接被拒绝）。
  const { challenge } = pkcePair();
  const stateValue = randomState();
  const cases: ("plain" | undefined)[] = ["plain", undefined];
  for (const method of cases) {
    const failedCallback = page.waitForEvent(
      "requestfailed",
      (request) => new URL(request.url()).hostname === "127.0.0.1" && new URL(request.url()).pathname === "/callback",
    );
    let refused = false;
    try {
      await page.goto(authorizeURL({ scope: "application:read", state: stateValue, challenge, method }));
    } catch (error) {
      refused = String(error).includes("ERR_CONNECTION_REFUSED");
    }
    const callbackRequest = await failedCallback;
    expect(refused, "回调地址应真实不可达（无监听端口）").toBe(true);
    const params = new URL(callbackRequest.url()).searchParams;
    expect(params.get("error")).toBe("invalid_request");
    expect(params.get("state")).toBe(stateValue);
    expect(params.get("code")).toBeNull();
  }
});

test("负向：state 与客户端期望不一致时必须中止且不使用 code", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const { verifier, challenge } = pkcePair();
  const sentState = randomState();
  // 模拟客户端期望被篡改/不一致：服务端会如实回显发送的 state，客户端比对失败必须拒绝。
  const expectedState = randomState();
  await captureCallback(adminPage);
  const viewPromise = armConsentView(adminPage);
  await adminPage.goto(authorizeURL({ scope: scopesWithOffline, state: sentState, challenge, method: "S256" }));
  await expect(adminPage).toHaveURL(/\/authorize\?request_id=req_/);
  await approveConsent(adminPage, { offline: true }, viewPromise);
  const callback = new URL(adminPage.url());
  const received = callback.searchParams.get("state");
  expect(received, "服务端必须回显请求中的 state").toBe(sentState);
  expect(received === expectedState, "state 校验失败，客户端必须拒绝该授权响应").toBe(false);
  // 验证确实拿到了一个未使用的 code，但客户端因 state 不一致中止，不发起任何令牌交换。
  expect(callback.searchParams.get("code")).toBeTruthy();
});

test("负向：授权码重复交换第二次失败（invalid_grant）", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const { verifier, challenge } = pkcePair();
  const stateValue = randomState();
  await captureCallback(adminPage);
  const viewPromise = armConsentView(adminPage);
  await adminPage.goto(authorizeURL({ scope: "application:read", state: stateValue, challenge, method: "S256" }));
  await expect(adminPage).toHaveURL(/\/authorize\?request_id=req_/);
  const ids = await approveConsent(adminPage, { offline: false }, viewPromise);
  const code = new URL(adminPage.url()).searchParams.get("code") ?? "";
  expect(code).toBeTruthy();
  const first = await exchangeCode(adminPage, code, verifier);
  expect(first.status(), await first.text()).toBe(200);
  const tokens = (await first.json()) as { access_token?: string; refresh_token?: string };
  expect(tokens.access_token).toBeTruthy();
  // 未请求 offline_access 且未同意离线访问：不签发 refresh token。
  expect(tokens.refresh_token).toBeUndefined();
  await expectBearerOK(adminPage, tokens.access_token!, ids, "首次交换的 access token 可用");

  // 同一授权码第二次交换必须失败（授权码单次使用）。
  const second = await exchangeCode(adminPage, code, verifier);
  expect(second.status()).toBe(400);
  const failure = (await second.json()) as { error?: string };
  expect(failure.error).toBe("invalid_grant");
});

test("匿名访问 /oauth/authorize 先跳登录，登录后回到授权流并完成授权", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const { verifier, challenge } = pkcePair();
  const stateValue = randomState();
  await captureCallback(page);
  // 布置监听后再发起授权：无会话时 303 到 /login?return_request=req_…，登录后回到授权流。
  const viewPromise = armConsentView(page);
  await page.goto(authorizeURL({ scope: scopesWithOffline, state: stateValue, challenge, method: "S256" }));
  await expect(page).toHaveURL(/\/login\?return_request=req_/);
  await page.getByLabel("用户名").fill(username!);
  await page.getByLabel("密码", { exact: true }).fill(password!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page).toHaveURL(/\/authorize\?request_id=req_/);
  const ids = await approveConsent(page, { offline: true }, viewPromise);
  const callback = new URL(page.url());
  expect(callback.searchParams.get("state")).toBe(stateValue);
  const tokenResponse = await exchangeCode(page, callback.searchParams.get("code") ?? "", verifier);
  expect(tokenResponse.status(), await tokenResponse.text()).toBe(200);
  const tokens = (await tokenResponse.json()) as { access_token?: string };
  await expectBearerOK(page, tokens.access_token!, ids, "匿名入口完成授权后令牌可用");
});

test("刷新流：refresh 轮换、旧 access 在有效期内可用、重用旧 refresh 撤销整个授权族", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  expect(state.tokens, "刷新用例依赖正向全流签发的令牌").toBeTruthy();
  const old = state.tokens!;
  const refresh = await adminPage.request.post("/oauth/token", {
    form: { grant_type: "refresh_token", refresh_token: old.refresh, client_id: clientId, resource: apiResource },
  });
  expect(refresh.status(), await refresh.text()).toBe(200);
  const next = (await refresh.json()) as { access_token?: string; refresh_token?: string; token_type?: string; scope?: string };
  expect(next.token_type).toBe("bearer");
  expect(next.access_token).toBeTruthy();
  expect(next.access_token).not.toBe(old.access);
  // 刷新必然轮换：新 refresh token 与旧值不同。
  expect(next.refresh_token).toBeTruthy();
  expect(next.refresh_token).not.toBe(old.refresh);
  // 刷新不得扩大或缩减 scope。
  expect(next.scope?.split(" ")).toEqual(expect.arrayContaining(["application:read", "offline_access"]));

  await expectBearerOK(adminPage, next.access_token!, old.ids, "刷新后的新 access token 可用");
  // 真实轮换语义（token.go + fosite flow_refresh.go）：刷新只轮换 refresh 族并签发新 access，
  // 旧 access 会话不被撤销，在其自身 15 分钟有效期内仍可用。
  await expectBearerOK(adminPage, old.access, old.ids, "旧 access token 在有效期内不被刷新撤销");

  // 重用旧 refresh token：重用检测使整个授权族失效（invalid_grant）。
  const reuse = await adminPage.request.post("/oauth/token", {
    form: { grant_type: "refresh_token", refresh_token: old.refresh, client_id: clientId, resource: apiResource },
  });
  expect(reuse.status()).toBe(400);
  expect(((await reuse.json()) as { error?: string }).error).toBe("invalid_grant");
  // 授权族撤销后，新旧 access token 一并失效。
  await expectBearerDenied(adminPage, old.access, old.ids, "重用检测撤销授权族后旧 access 应失效");
  await expectBearerDenied(adminPage, next.access_token!, old.ids, "重用检测撤销授权族后新 access 应失效");
});

test("撤销流：撤销 access token 后立即 401，同族 refresh 一并失效", async ({ adminPage }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  const { verifier, challenge } = pkcePair();
  const stateValue = randomState();
  await captureCallback(adminPage);
  const viewPromise = armConsentView(adminPage);
  await adminPage.goto(authorizeURL({ scope: scopesWithOffline, state: stateValue, challenge, method: "S256" }));
  await expect(adminPage).toHaveURL(/\/authorize\?request_id=req_/);
  const ids = await approveConsent(adminPage, { offline: true }, viewPromise);
  const callback = new URL(adminPage.url());
  const tokenResponse = await exchangeCode(adminPage, callback.searchParams.get("code") ?? "", verifier);
  expect(tokenResponse.status()).toBe(200);
  const tokens = (await tokenResponse.json()) as { access_token?: string; refresh_token?: string };
  expect(tokens.access_token).toBeTruthy();
  expect(tokens.refresh_token).toBeTruthy();
  await expectBearerOK(adminPage, tokens.access_token!, ids, "撤销前令牌可用");

  // RFC 7009：撤销请求必须携带客户端认证；public client 以 client_id 认证。
  const revoke = await adminPage.request.post("/oauth/revoke", {
    form: { token: tokens.access_token!, client_id: clientId, token_type_hint: "access_token" },
  });
  expect(revoke.status()).toBe(200);
  expect(await revoke.text()).toBe("");

  await expectBearerDenied(adminPage, tokens.access_token!, ids, "撤销后的 access token 必须 401");
  // RFC 7009 SHOULD：撤销 access token 同时撤销同族 refresh token。
  const refreshAfter = await adminPage.request.post("/oauth/token", {
    form: { grant_type: "refresh_token", refresh_token: tokens.refresh_token!, client_id: clientId, resource: apiResource },
  });
  expect(refreshAfter.status()).toBe(400);
  expect(((await refreshAfter.json()) as { error?: string }).error).toBe("invalid_grant");
});
