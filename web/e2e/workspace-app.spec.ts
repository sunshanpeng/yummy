// 工作空间 → 应用真实创建流、运行配置保存与部署分离文案、构建前置、个人 PAT 全生命周期。
// 这些步骤共享同一条真实数据链路（空间/应用/令牌互相依赖），因此保持单个顺序用例；
// 全部断言基于真实控制面的持久化结果（刷新后复核），凭证只读环境变量，不写死。
import { expect, type Page, test } from "@playwright/test";

const username = process.env.YUMMY_E2E_USERNAME;
const password = process.env.YUMMY_E2E_PASSWORD;
const run = Date.now().toString(36);
const workspaceName = `e2e 空间 ${run}`;
const slug = `e2e-ws-${run}`;
const appName = `e2e-app-${run}`;
const tokenName = `e2e-token-${run}`;
// 执行节点未注册、平台执行策略未配置时，控制面对执行资格查询返回的具体前置文案。
const executionNotReady = "实例尚未配置执行策略";

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

// 工作空间/令牌等列表随运行累积且分页每页 24 条；目标行可能落在后续页，逐页查找。
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

test("工作空间、应用、运行配置、构建前置与个人令牌的真实跨页流程", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  await login(page);

  // 工作空间从账户菜单的全局管理创建（实例级入口），slug 带时间戳保证真实唯一性。
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "全局管理" }).click();
  await expect(page.getByRole("heading", { name: "全部工作空间" })).toBeVisible();
  await page.getByRole("button", { name: "创建空间" }).click();
  await page.getByLabel("工作空间名称").fill(workspaceName);
  await page.getByLabel("空间标识").fill(slug);
  await page.getByLabel("空间说明").fill("E2E 自动化创建的工作空间。");
  await page.getByRole("button", { name: "创建工作空间" }).click();
  await expect(await findRow(page, new RegExp(slug), "新建工作空间")).toBeVisible({ timeout: 10000 });
  // 刷新复核：工作空间已真实持久化。
  await page.reload();
  await expect(await findRow(page, new RegExp(slug), "新建工作空间")).toBeVisible({ timeout: 10000 });

  // 从日常首页进入空间首页（只有应用列表，无全局构建入口）。
  await page.goto("/");
  await page.getByRole("link").filter({ hasText: slug }).click();
  await expect(page.getByRole("heading", { name: "应用", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "从你的第一个应用开始" })).toBeVisible();

  // 创建应用只填写名称；创建后直接进入该应用的运行配置页，不触发构建或部署。
  await page.getByRole("button", { name: "创建应用" }).click();
  await page.getByLabel("应用名称").fill(appName);
  await page.getByRole("button", { name: "创建并配置" }).click();
  await page.waitForURL(/\/settings\/runtime$/, { timeout: 15000 });
  await expect(page.getByRole("heading", { name: appName })).toBeVisible({ timeout: 10000 });
  await expect(page.getByRole("heading", { name: "运行配置" })).toBeVisible();

  // 保存与部署生效分离：保存前给出明确文案。
  await expect(page.getByText("保存只创建配置版本，不会构建或部署。部署需在应用部署页明确确认；部署后可在部署页查看运行日志与基础监控。")).toBeVisible();
  await expect(page.getByText("当前为系统建议，尚未保存。")).toBeVisible();
  // 执行资格前置：平台策略/节点未就绪时如实展示控制面错误，而不是暗示可以部署。
  await expect(page.getByText(executionNotReady)).toBeVisible();

  await page.getByRole("button", { name: "保存运行配置" }).click();
  await expect(page.getByText("最新已保存配置 v1。此版本不代表正在运行。")).toBeVisible({ timeout: 10000 });

  // 刷新复核：运行配置 v1 已真实持久化，且仍不表示正在运行。
  await page.reload();
  await expect(page.getByRole("heading", { name: "运行配置" })).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("最新已保存配置 v1。此版本不代表正在运行。")).toBeVisible();

  // 构建页真实空态；创建构建页如实展示执行资格前置错误，不出现“资格通过”。
  await page.getByRole("link", { name: "构建", exact: true }).click();
  await expect(page.getByRole("heading", { name: "构建记录", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "尚无构建记录" })).toBeVisible();
  await expect(page.getByText("先上传源码快照并确认构建配置，再预览实际执行资源。没有任务时不会展示示例构建。")).toBeVisible();
  await page.getByRole("link", { name: "创建构建" }).click();
  await expect(page.getByRole("heading", { name: "创建构建", exact: true })).toBeVisible();
  await expect(page.getByText(executionNotReady)).toBeVisible();
  await expect(page.getByText("服务端当前检查通过")).toHaveCount(0);

  // 响应式抽查：375/1440 两档无横向溢出（应用详情页）。
  await expectNoHorizontalOverflow(page, 375);
  await expectNoHorizontalOverflow(page, 1440);

  // —— 个人 PAT：从账户菜单进入个人账户域，授权目标用上面真实创建的空间和应用。——
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "个人账户", exact: true }).click();
  await expect(page.getByRole("heading", { name: "个人账户", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "切换工作空间" })).toHaveCount(0);
  await page.getByRole("link", { name: "个人访问令牌", exact: true }).click();
  await expect(page.getByRole("button", { name: "创建令牌" })).toBeVisible();

  await page.getByRole("button", { name: "创建令牌" }).click();
  await page.getByLabel("令牌名称").fill(tokenName);
  await page.getByLabel("有效期").selectOption({ label: "7 天" });
  await page.locator("#token-workspace").selectOption({ label: workspaceName });
  await page.getByRole("checkbox", { name: new RegExp(appName) }).check();
  await page.locator("form").getByRole("button", { name: "创建令牌" }).click();

  // 明文一次性展示：页面恰好出现一次，标题明确不可再次查看。
  const secret = page.locator("section[aria-label='个人访问令牌已创建']");
  await expect(secret).toBeVisible({ timeout: 10000 });
  await expect(secret.getByText("此内容只显示一次，离开或关闭后无法再次查看。请通过安全渠道保存或分享。")).toBeVisible();
  const plaintext = (await secret.locator("code").textContent()) ?? "";
  expect(plaintext).toMatch(/^ym_pat_/);
  expect(await page.getByText(plaintext).count(), "令牌明文应只在页面出现一次").toBe(1);
  const tokenRow = await findRow(page, new RegExp(tokenName), "新建访问令牌");
  await expect(tokenRow).toBeVisible();
  await expect(tokenRow.getByText("application:read")).toBeVisible();

  // 刷新复核：列表持久化，明文不可再现。
  await page.reload();
  await expect(page.getByRole("table", { name: "个人访问令牌" })).toBeVisible({ timeout: 10000 });
  await expect(await findRow(page, new RegExp(tokenName), "新建访问令牌")).toBeVisible();
  expect(await page.getByText(plaintext).count(), "刷新后明文不应再次出现").toBe(0);

  // 撤销走确认弹窗，撤销后状态真实持久化。
  await page.getByRole("button", { name: "撤销", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("dialog").getByText(`撤销「${tokenName}」后，使用该凭证的 CLI 或 Agent 将无法继续请求。不会自动取消已有任务。`)).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "确认" }).click();
  await expect((await findRow(page, new RegExp(tokenName), "新建访问令牌")).getByText("已撤销")).toBeVisible({ timeout: 10000 });
  await page.reload();
  await expect((await findRow(page, new RegExp(tokenName), "新建访问令牌")).getByText("已撤销")).toBeVisible({ timeout: 10000 });

  // 响应式抽查：375/1440 两档无横向溢出（个人账户页）。
  await expectNoHorizontalOverflow(page, 375);
  await expectNoHorizontalOverflow(page, 1440);
});
