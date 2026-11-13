// 实例管理员（全局管理）跨页流程：管理入口、页签切换、OAuth 客户端全生命周期。
// 全部断言基于真实控制面的持久化结果（刷新后复核），凭证只读环境变量，不写死。
import { expect, type Page, test } from "@playwright/test";

const username = process.env.YUMMY_E2E_USERNAME;
const password = process.env.YUMMY_E2E_PASSWORD;
// 本实例 PublicBaseURL 与测试 base URL 一致，OAuth 资源必须是本实例 API 资源或应用 MCP 规范地址。
const origin = new URL(process.env.YUMMY_E2E_BASE_URL ?? "http://127.0.0.1:8080").origin;
const run = Date.now().toString(36);
// 测试数据跨运行累积且列表按 id ASC 每页 24 条；目标行可能落在后续页。
// 逐页（最多 12 页）查找目标行，找不到时以明确错误失败而不是误报元素不存在。
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

const clientId = `e2e-client-${run}`;

async function login(page: Page) {
  await page.goto("/login");
  await page.getByLabel("用户名").fill(username!);
  await page.getByLabel("密码", { exact: true }).fill(password!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "你的工作空间" })).toBeVisible();
}

async function expectNoHorizontalOverflow(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${width}px 不应出现横向溢出`).toBe(true);
}

// 页签内容可能是空态或非空表格，两者都是该页签的正确呈现，分别断言各自真实文案。
async function expectTabShowsEmptyOrTable(page: Page, emptyTitle: string, tableLabel: string) {
  await expect(page.getByRole("heading", { name: emptyTitle }).or(page.getByRole("table", { name: tableLabel }))).toBeVisible();
}

test("实例管理员经账户菜单进入全局管理并切换全部页签", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  await login(page);

  // 全局管理入口只应出现在账户菜单里（实例管理员可见）。
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "全局管理" }).click();
  await expect(page.getByRole("heading", { name: "全局管理" })).toBeVisible();
  // /platform 重定向到工作空间页签，工作空间页签成为当前页。
  await expect(page).toHaveURL(/\/platform\/workspaces$/);
  await expect(page.getByRole("link", { name: "工作空间", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { name: "全部工作空间" })).toBeVisible();
  await expectTabShowsEmptyOrTable(page, "尚无工作空间", "全部工作空间");

  const tabs: [string, () => Promise<void>][] = [
    ["用户与安全", async () => {
      await expect(page.getByRole("table", { name: "实例用户" })).toBeVisible();
      // 实例管理员能看到自己：实例身份、正常状态，以及修复后的「停用用户」操作。
      const adminRow = page.getByRole("row", { name: new RegExp(username!) });
      await expect(adminRow).toBeVisible();
      await expect(adminRow.getByText("实例管理员")).toBeVisible();
      await expect(adminRow.getByText("正常", { exact: true })).toBeVisible();
      await expect(adminRow.getByRole("button", { name: "停用用户" })).toBeVisible();
    }],
    ["执行节点", () => expectTabShowsEmptyOrTable(page, "尚未登记节点", "实例执行节点")],
    ["输出仓库", () => expectTabShowsEmptyOrTable(page, "尚未配置输出仓库", "实例输出仓库")],
    ["平台策略", async () => {
      await expect(page.getByRole("heading", { name: "平台执行策略" })).toBeVisible();
      await expect(page.getByText("此策略统一控制构建节点、首次运行节点、输出仓库、平台根域名与资源默认值。")).toBeVisible();
    }],
    ["资源分配", async () => {
      await expect(page.getByRole("heading", { name: "工作空间资源分配" })).toBeVisible();
      await expect(page.getByLabel("分配目标空间")).toBeVisible();
    }],
    ["OAuth 客户端", async () => {
      await expect(page.getByRole("table", { name: "OAuth 客户端" })).toBeVisible();
      // 第一方 CLI 客户端真实存在于列表且处于启用状态。
      const cliRow = await findRow(page, /yummy-cli/, "第一方 CLI 客户端");
      await expect(cliRow).toBeVisible();
      await expect(cliRow.getByText("已启用")).toBeVisible();
    }],
  ];
  for (const [tab, assertTab] of tabs) {
    await page.getByRole("link", { name: tab, exact: true }).click();
    await expect(page.getByRole("link", { name: tab, exact: true })).toHaveAttribute("aria-current", "page");
    await assertTab();
  }

  // 响应式抽查：375/1440 两档无横向溢出。
  await expectNoHorizontalOverflow(page, 375);
  await expectNoHorizontalOverflow(page, 1440);
});

test("OAuth 客户端注册、停用、重新启用、再次停用均真实持久化", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  await login(page);
  const clientName = `e2e 客户端 ${run}`;

  await page.goto("/platform/oauth-clients");
  await expect(page.getByRole("table", { name: "OAuth 客户端" })).toBeVisible();
  await page.getByRole("button", { name: "注册客户端" }).click();
  await page.getByLabel("客户端 ID（client_id）").fill(clientId);
  await page.getByLabel("客户端名称").fill(clientName);
  await page.getByLabel("回调地址（每行一个）").fill("https://127.0.0.1/callback");
  // 页面提示范围可留空，但当前实例对空范围会返回内部错误（已记录的真实缺陷），
  // 因此按页面上实例真实发放的范围填写，走可成功的正向路径。
  await page.getByLabel("授权范围（空格或逗号分隔，可留空）").fill("application:read");
  await page.getByLabel("资源（每行一个）").fill(`${origin}/api`);
  await page.getByRole("button", { name: "登记客户端" }).click();

  const row = await findRow(page, new RegExp(clientId), "本用例注册的客户端");
  await expect(row).toBeVisible({ timeout: 10000 });
  await expect(row.getByText(clientName)).toBeVisible();
  await expect(row.getByText("预登记")).toBeVisible();
  await expect(row.getByText("https://127.0.0.1/callback")).toBeVisible();
  await expect(row.getByText("已启用")).toBeVisible();

  // 刷新复核：注册结果已真实持久化。
  await page.reload();
  await expect((await findRow(page, new RegExp(clientId), "本用例注册的客户端")).getByText("已启用")).toBeVisible({ timeout: 10000 });

  async function setActive(buttonName: string, expectBadge: string) {
    const current = await findRow(page, new RegExp(clientId), "本用例注册的客户端");
    await current.getByRole("button", { name: buttonName }).click();
    // 确认弹窗展示该客户端的后果说明后才执行。
    await expect(page.getByRole("dialog")).toBeVisible();
    await expect(page.getByRole("dialog").getByText(clientName)).toBeVisible();
    await page.getByRole("dialog").getByRole("button", { name: "确认" }).click();
    await expect((await findRow(page, new RegExp(clientId), "本用例注册的客户端")).getByText(expectBadge)).toBeVisible({ timeout: 10000 });
  }

  await setActive("停用客户端", "已停用");
  // 刷新复核：停用已持久化。
  await page.reload();
  await expect((await findRow(page, new RegExp(clientId), "本用例注册的客户端")).getByText("已停用")).toBeVisible({ timeout: 10000 });

  await setActive("启用客户端", "已启用");
  await setActive("停用客户端", "已停用");
  // 刷新复核：再次停用仍保持已停用。
  await page.reload();
  await expect((await findRow(page, new RegExp(clientId), "本用例注册的客户端")).getByText("已停用")).toBeVisible({ timeout: 10000 });

  await expectNoHorizontalOverflow(page, 375);
  await expectNoHorizontalOverflow(page, 1440);
});
