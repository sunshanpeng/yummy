// 邀请与权限门控真实流：空间管理员经工作空间管理「邀请」页创建真实邀请链接
// （一次性 ym_inv_ 令牌）→ 新浏览器上下文消费链接并设置密码激活 → 新成员登录 →
// 按服务端授权语义断言真实可见性（access.go：普通成员必须被加入应用才可见应用，
// 列表在查询阶段过滤，不是前端隐藏）→ 管理员在应用「成员与权限」授予应用角色 →
// 可见性真实变化。凭证只读环境变量；邀请链接只在内存流转，不写文件。
import { expect, type Page, test } from "@playwright/test";

const adminUsername = process.env.YUMMY_E2E_USERNAME;
const adminPassword = process.env.YUMMY_E2E_PASSWORD;
const baseURL = process.env.YUMMY_E2E_BASE_URL ?? "http://127.0.0.1:8080";
const origin = new URL(baseURL).origin;
const run = Date.now().toString(36);
const workspaceName = `e2e 权限空间 ${run}`;
const slug = `e2e-perm-${run}`;
const appName = `e2e-perm-app-${run}`;
// 邀请创建的新成员账号与密码（服务端要求密码 12～128 字节）。
const memberUsername = `e2e-member-${run}`;
const memberPassword = `e2e-member-pass-${run}`;

async function login(page: Page, username?: string, password?: string) {
  await page.goto("/login");
  await page.getByLabel("用户名").fill(username ?? adminUsername!);
  await page.getByLabel("密码", { exact: true }).fill(password ?? adminPassword!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "你的工作空间" })).toBeVisible();
}

// 列表随运行累积且分页每页 24 条；目标行可能落在后续页，逐页查找。
async function findRow(page: Page, pattern: RegExp, label: string) {
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

test("邀请成员、一次性激活、登录与权限门控真实流", async ({ page, browser }) => {
  test.skip(!adminUsername || !adminPassword, "由父级提供真实实例的 E2E 用户环境变量");
  // 流程覆盖两个登录会话与多次跨页等待，默认 30 秒不够。
  test.setTimeout(120_000);
  await login(page);

  // 自建专用空间与应用，避免依赖其他 spec 的运行数据。
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "全局管理" }).click();
  await expect(page.getByRole("heading", { name: "全部工作空间" })).toBeVisible();
  await page.getByRole("button", { name: "创建空间" }).click();
  await page.getByLabel("工作空间名称").fill(workspaceName);
  await page.getByLabel("空间标识").fill(slug);
  await page.getByLabel("空间说明").fill("E2E 邀请与权限用工作空间。");
  await page.getByRole("button", { name: "创建工作空间" }).click();
  // 表单关闭 = 创建 RPC 已真实成功返回；此后刷新才不会打断在途请求。
  await expect(page.getByRole("button", { name: "取消创建" })).toHaveCount(0, { timeout: 10000 });
  // 刷新后列表与游标同代，跨页查找新空间才可靠（创建触发的局部刷新与
  // findRow 的翻页存在时序竞态，数据量跨过单页 24 条后不稳定，已如实记录）。
  await page.reload();
  await expect(page.getByRole("table", { name: "全部工作空间" }).or(page.getByText("尚无工作空间"))).toBeVisible({ timeout: 10000 });
  await expect(await findRow(page, new RegExp(slug), "新建工作空间")).toBeVisible({ timeout: 10000 });

  await page.goto("/");
  await page.getByRole("link").filter({ hasText: slug }).click();
  await page.getByRole("button", { name: "创建应用" }).click();
  await page.getByLabel("应用名称").fill(appName);
  await page.getByRole("button", { name: "创建并配置" }).click();
  await page.waitForURL(/\/settings\/runtime$/, { timeout: 15000 });
  const appPath = page.url().replace(/\/settings\/runtime$/, "");

  // 管理员经空间管理「邀请」页创建真实邀请链接（UI 真实入口）。
  await page.getByRole("link", { name: "返回应用" }).click();
  await page.getByRole("link", { name: "空间管理" }).click();
  await expect(page.getByRole("heading", { name: "工作空间管理" })).toBeVisible();
  await page.getByRole("link", { name: "邀请", exact: true }).click();
  await expect(page.getByRole("heading", { name: "邀请成员" })).toBeVisible();
  await page.getByLabel("受邀用户名").fill(memberUsername);
  await page.getByLabel("空间角色").selectOption({ label: "普通成员" });
  await page.getByRole("button", { name: "创建邀请链接" }).click();

  // 链接只展示一次：明文含一次性 ym_inv_ 令牌，关闭后不可再现。
  const secret = page.locator("section[aria-label='邀请链接已创建']");
  await expect(secret).toBeVisible({ timeout: 10000 });
  const inviteLink = (await secret.locator("code").textContent())!.trim();
  expect(inviteLink.startsWith(`${origin}/accept-invite#token=ym_inv_`), "邀请链接应包含一次性令牌").toBe(true);
  await expect(await findRow(page, new RegExp(memberUsername), "新建邀请")).toBeVisible();
  await secret.getByRole("button", { name: "已保存，关闭" }).click();
  await expect(secret).toHaveCount(0);
  expect(await page.getByText("ym_inv_").count(), "关闭后明文令牌不可再现").toBe(0);

  // 新浏览器上下文消费邀请链接：真实设置密码激活新账户。
  const memberContext = await browser.newContext();
  const memberPage = await memberContext.newPage();
  await memberPage.goto(inviteLink);
  await expect(memberPage.getByRole("heading", { name: "加入工作空间" })).toBeVisible();
  await expect(memberPage.getByLabel("邀请令牌")).toHaveValue(/^ym_inv_/);
  // 真实发现：接受邀请页“账户密码”字段前端只校验非空（invite 模式未用 passwordSchema），
  // 短密码会真实到达服务端并被 Argon2 长度校验拒绝；先断言服务端文案，再完成激活。
  await memberPage.getByLabel("账户密码").fill("e2e-short");
  await memberPage.getByRole("button", { name: "接受邀请" }).click();
  await expect(memberPage.getByText("密码必须为 12～128 个字节")).toBeVisible({ timeout: 10000 });
  await memberPage.getByLabel("账户密码").fill(memberPassword);
  await memberPage.getByRole("button", { name: "接受邀请" }).click();
  await expect(memberPage.getByText("邀请已接受。请登录后进入工作空间。")).toBeVisible();

  // 邀请一次性：同一链接再次消费被服务端拒绝（令牌已消费，查询直接未命中）。
  // 先离开 /accept-invite 再回来：接受成功后 SPA 已把 URL 替换为无 hash 的
  // /accept-invite，直接 goto 邀请链接只是 hash 变化，组件不会重新挂载。
  await memberPage.goto("/login");
  await memberPage.goto(inviteLink);
  await memberPage.getByLabel("账户密码").fill(memberPassword);
  await memberPage.getByRole("button", { name: "接受邀请" }).click();
  await expect(memberPage.getByText("资源不存在、已归档，或当前账户无权访问。")).toBeVisible();

  // 新成员登录（新账号，独立频控配额）。
  await login(memberPage, memberUsername, memberPassword);

  // 权限门控：成员可见被邀请的空间，但看不到任何未授权应用（查询阶段过滤的空态）。
  await memberPage.getByRole("link").filter({ hasText: slug }).click();
  await expect(memberPage.getByRole("heading", { name: "从你的第一个应用开始" })).toBeVisible();
  await expect(memberPage.getByText("当前没有已授权的应用。请联系空间管理员获得应用访问权限。")).toBeVisible();
  await expect(memberPage.getByRole("button", { name: "创建应用" })).toHaveCount(0);
  await expect(memberPage.getByRole("link", { name: "空间管理" })).toHaveCount(0);
  // 直接访问应用详情 URL 同样被拒（服务端 NotFound，不是前端隐藏）。
  await memberPage.goto(appPath);
  await expect(memberPage.getByText("资源不存在、已归档，或当前账户无权访问。")).toBeVisible();
  await expect(memberPage.getByRole("heading", { name: appName })).toHaveCount(0);
  // 普通成员的账户菜单没有全局管理入口（仅实例管理员可见）。
  await memberPage.goto("/");
  await memberPage.getByRole("button", { name: "账户菜单" }).click();
  await expect(memberPage.getByRole("menuitem", { name: "全局管理" })).toHaveCount(0);
  await memberPage.keyboard.press("Escape");

  // 管理员在应用「成员与权限」把新成员授为开发者（应用配置中的真实授权入口）。
  await page.goto(`${appPath}/settings/access`);
  await expect(page.getByRole("heading", { name: "成员与权限" })).toBeVisible();
  const memberOption = page.getByLabel("空间成员").locator("option").filter({ hasText: memberUsername });
  const optionValue = await memberOption.getAttribute("value");
  expect(optionValue, "空间成员下拉应包含受邀新成员").toBeTruthy();
  await page.getByLabel("空间成员").selectOption(optionValue!);
  await page.getByLabel("应用角色").selectOption({ label: "开发者" });
  await page.getByRole("button", { name: "保存应用授权" }).click();
  await expect(page.getByText("应用授权已保存。")).toBeVisible();
  const grantRow = await findRow(page, new RegExp(memberUsername), "应用成员授权行");
  await expect(grantRow.getByText("开发者")).toBeVisible();

  // 新成员复核：应用可见性真实变化（可进入应用详情，具备开发者能力入口，
  // 但仍没有成员与权限管理入口——developer 角色不含 member:write）。
  await memberPage.goto("/");
  await memberPage.getByRole("link").filter({ hasText: slug }).click();
  const appCard = memberPage.getByRole("link").filter({ hasText: appName });
  await expect(appCard).toBeVisible({ timeout: 10000 });
  await appCard.click();
  await expect(memberPage.getByRole("heading", { name: appName })).toBeVisible({ timeout: 10000 });
  await expect(memberPage.getByRole("link", { name: "构建", exact: true })).toBeVisible();
  await expect(memberPage.getByRole("link", { name: "源码", exact: true })).toBeVisible();
  await expect(memberPage.getByRole("link", { name: "成员与权限" })).toHaveCount(0);

  await memberContext.close();
});
