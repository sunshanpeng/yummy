import { expect, test } from "@playwright/test";

const username = process.env.YUMMY_E2E_USERNAME;
const password = process.env.YUMMY_E2E_PASSWORD;
for (const width of [375, 768, 1024, 1440]) {
  test(`登录页在 ${width}px 无横向溢出且键盘可用`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/login");
    await expect(page.getByRole("heading", { name: "登录 Yummy" })).toBeVisible();
    await expect(page.getByLabel("用户名")).toBeVisible();
    await expect(page.getByLabel("密码", { exact: true })).toBeVisible();
    await page.keyboard.press("Tab");
    await expect(page.getByRole("link", { name: "跳转到主要内容" })).toBeFocused();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
}

test("真实登录、独立账户域和退出会话", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  await page.goto("/login");
  await page.getByLabel("用户名").fill(username!);
  await page.getByLabel("密码", { exact: true }).fill(password!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "你的工作空间" })).toBeVisible();
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "个人账户", exact: true }).click();
  await expect(page.getByRole("heading", { name: "个人账户", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "切换工作空间" })).toHaveCount(0);
  await page.getByRole("link", { name: "个人访问令牌", exact: true }).click();
  await expect(page.getByRole("button", { name: "创建令牌" })).toBeVisible();
  await expect(page.getByRole("button", { name: "切换工作空间" })).toHaveCount(0);
  for (const width of [375, 768, 1024, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  }
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "退出登录" }).click();
  await expect(page.getByRole("heading", { name: "登录 Yummy" })).toBeVisible();
  await page.goto("/account/tokens");
  await expect(page.getByRole("heading", { name: "登录 Yummy" })).toBeVisible();
});
