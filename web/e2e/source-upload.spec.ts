// 源码安全上传真实流：合法 tar.gz 上传、快照 READY、SHA-256/大小/条目核对、
// 创建构建页如实展示执行资格前置；路径逃逸与敏感路径包被服务端真实拒绝。
// 归档在 Node 内按 tar 规范手工编码（系统 tar 会规范化或拒绝 `../` 这类非法成员名），
// 通过 setInputFiles 以内存 buffer 提交，全程不产生临时文件。
// 服务端校验语义依据 internal/platform/sourcearchive/inspect.go 与 policy.go：
// 完整校验发生在 completeSourceUpload（Inspect），拒绝文案来自 connect 错误消息原文。
import { createHash } from "node:crypto";
import { gzipSync } from "node:zlib";
import { expect, type Page, test } from "@playwright/test";

const username = process.env.YUMMY_E2E_USERNAME;
const password = process.env.YUMMY_E2E_PASSWORD;
const baseURL = process.env.YUMMY_E2E_BASE_URL ?? "http://127.0.0.1:8080";
// 控制面把源码传输入口独立监听在 --transfer-addr（默认 127.0.0.1:8081），
// 生产部署由反向代理把同源 /transfer/ 转发到该入口；本地 e2e 实例没有反向代理。
const transferOrigin = process.env.YUMMY_E2E_TRANSFER_ORIGIN ?? "http://127.0.0.1:8081";
const run = Date.now().toString(36);
const workspaceName = `e2e 源码空间 ${run}`;
const slug = `e2e-src-${run}`;
const appName = `e2e-src-app-${run}`;
// 执行节点未注册、平台执行策略未配置时，控制面对执行资格查询返回的具体前置文案。
const executionNotReady = "实例尚未配置执行策略";

// 按 ustar 规范编码单个普通文件成员：512 字节头 + 内容 + 512 对齐填充。
function tarEntry(name: string, body: Buffer): Buffer {
  const header = Buffer.alloc(512, 0);
  header.write(name, 0, "utf8");
  header.write("0000644\0", 100, "utf8"); // mode
  header.write("0000000\0", 108, "utf8"); // uid
  header.write("0000000\0", 116, "utf8"); // gid
  header.write(`${body.length.toString(8).padStart(11, "0")}\0`, 124, "utf8"); // size
  header.write(`${Math.floor(Date.now() / 1000).toString(8).padStart(11, "0")}\0`, 136, "utf8"); // mtime
  header.write("        ", 148, "utf8"); // chksum 先以空格占位参与求和
  header.write("0", 156, "utf8"); // typeflag：普通文件
  header.write("ustar\0", 257, "utf8");
  header.write("00", 263, "utf8");
  let sum = 0;
  for (const byte of header) sum += byte;
  header.write(`${sum.toString(8).padStart(6, "0")}\0 `, 148, "utf8");
  const padded = Buffer.alloc(Math.ceil(body.length / 512) * 512);
  body.copy(padded);
  return Buffer.concat([header, padded]);
}

// gzip 压缩手工 tar：两个全零结束块并补齐到 10240 字节记录大小，尾部保持全零。
function makeTarGz(entries: { name: string; body: string }[]): { buffer: Buffer; sha256: string; expanded: number } {
  const body = Buffer.concat(entries.map((entry) => tarEntry(entry.name, Buffer.from(entry.body, "utf8"))));
  const padded = Buffer.alloc(Math.ceil((body.length + 1024) / 10240) * 10240);
  body.copy(padded);
  const buffer = gzipSync(padded);
  return { buffer, sha256: createHash("sha256").update(buffer).digest("hex"), expanded: padded.length };
}

// 合法包：构建上下文内包含 Dockerfile 与一个小文件（≤ 服务端 256 MiB 限制）。
const legal = makeTarGz([
  { name: "Dockerfile", body: `FROM alpine:3.20\nCOPY main.py /app/main.py\n` },
  { name: "main.py", body: `print("e2e source ${run}")\n` },
]);
// 纯源码包：没有 Dockerfile，构建时由 Railpack 识别。上传阶段不要求 Dockerfile。
const sourceOnly = makeTarGz([
  { name: "package.json", body: `{"name":"e2e-${run}"}\n` },
  { name: "index.js", body: `console.log("e2e source only ${run}")\n` },
]);
// 恶意包 1：tar 成员名路径逃逸（../），服务端 Inspect 必须在 complete 阶段拒绝。
const evilPath = makeTarGz([{ name: "../evil.txt", body: "e2e path escape probe" }]);
// 恶意包 2：平台排除的敏感路径 .env，验证敏感文件拒绝规则真实生效。
const sensitiveFile = makeTarGz([
  { name: "Dockerfile", body: "FROM alpine:3.20\n" },
  { name: ".env", body: `E2E_DEMO_SECRET=${run}\n` },
]);

async function login(page: Page) {
  await page.goto("/login");
  await page.getByLabel("用户名").fill(username!);
  await page.getByLabel("密码", { exact: true }).fill(password!);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("heading", { name: "你的工作空间" })).toBeVisible();
}

// 真实发现（详见测试报告）：前端 uploadSource 用同源相对路径 fetch /transfer/，
// 而控制面将传输入口独立监听在 8081 且要求 Host 与 PublicBaseURL 一致；
// 本地实例无反向代理，页面上传必然 404（8080 无该路由）。
// 此处仅做与生产反代等价的最小网络寻址转发（协议、会话 Cookie、CSRF 头、
// 请求体与服务端校验全部保持真实），不属于对被测行为的篡改。
async function proxyTransfer(page: Page) {
  await page.route("**/transfer/**", async (route) => {
    const url = new URL(route.request().url());
    await route.fulfill({ response: await route.fetch({ url: `${transferOrigin}${url.pathname}`, headers: { ...route.request().headers(), host: new URL(baseURL).host } }) });
  });
}

// 工作空间列表随运行累积且分页每页 24 条；目标行可能落在后续页，逐页查找。
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

// 源码页 metadata（dl.metadata）中某一 dt 对应的 dd 值。
function metaValue(page: Page, label: string) {
  return page.locator("dl.metadata > div").filter({ hasText: label }).locator("dd");
}

async function upload(page: Page, archive: { buffer: Buffer }, fileName: string, dockerfile = "") {
  await page.setInputFiles("#source-file", { name: fileName, mimeType: "application/gzip", buffer: archive.buffer });
  // Dockerfile 路径是可选项：留空表示不指定，构建时自动选择；显式填写则要求归档中存在该文件。
  await page.getByLabel("Dockerfile 相对路径").fill(dockerfile);
  await page.getByRole("button", { name: "上传并校验源码" }).click();
}

test("源码安全上传、快照核对与恶意包真实拒绝", async ({ page }) => {
  test.skip(!username || !password, "由父级提供真实实例的 E2E 用户环境变量");
  await proxyTransfer(page);
  await login(page);

  // 自建专用空间与应用（跨 spec 不依赖其他用例的时间戳数据）。
  await page.getByRole("button", { name: "账户菜单" }).click();
  await page.getByRole("menuitem", { name: "全局管理" }).click();
  await expect(page.getByRole("heading", { name: "全部工作空间" })).toBeVisible();
  await page.getByRole("button", { name: "创建空间" }).click();
  await page.getByLabel("工作空间名称").fill(workspaceName);
  await page.getByLabel("空间标识").fill(slug);
  await page.getByLabel("空间说明").fill("E2E 源码上传用工作空间。");
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

  // 进入源码页：固定文案如实说明上传只准备快照，不触发构建或部署。
  await page.getByRole("link", { name: "源码", exact: true }).click();
  await expect(page.getByRole("heading", { name: "源码上传与校验" })).toBeVisible();
  await expect(page.getByText("上传完成只表示源码快照已准备好，不代表镜像产出或应用上线。请到应用构建页预览输入后明确创建构建。")).toBeVisible();

  // 合法包真实上传：浏览器计算 SHA-256，服务端 Inspect 校验归档后返回 READY 快照。
  await upload(page, legal, `e2e-src-${run}.tar.gz`, "Dockerfile");
  await expect(page.getByText("源码快照状态：READY")).toBeVisible({ timeout: 15000 });
  const snapshotId = (await metaValue(page, "快照 ID").locator("code").textContent())!.trim();
  expect(snapshotId).toMatch(/^src_[0-9a-f]{32}$/);
  await expect(metaValue(page, "Dockerfile").locator("code")).toHaveText("Dockerfile");
  // 压缩/展开大小与 SHA-256 必须与服务端实际校验值一致（本地独立计算比对）。
  await expect(metaValue(page, "压缩 / 展开大小")).toHaveText(`${legal.buffer.length} / ${legal.expanded} 字节`);
  await expect(metaValue(page, "条目数量 / 到期")).toContainText("2 · ");
  await expect(metaValue(page, "SHA-256").locator("code")).toHaveText(legal.sha256);
  await expect(page.getByRole("row", { name: /Dockerfile/ })).toBeVisible();
  await expect(page.getByRole("row", { name: /main\.py/ })).toBeVisible();

  // 使用该快照进入创建构建页：如实展示执行资格前置，不出现“资格通过”。
  await page.getByRole("link", { name: "使用此快照创建构建" }).click();
  await expect(page.getByRole("heading", { name: "创建构建", exact: true })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`builds/new\\?snapshotId=${snapshotId}$`));
  await expect(page.getByLabel("源码快照 ID")).toHaveValue(snapshotId);
  await expect(page.getByText(executionNotReady)).toBeVisible();
  await expect(page.getByText("服务端当前检查通过")).toHaveCount(0);

  // 恶意包 1（路径逃逸）：服务端在 complete 阶段真实拒绝，展示原始拒绝文案。
  await page.getByRole("link", { name: "源码", exact: true }).click();
  await expect(page.getByRole("heading", { name: "源码上传与校验" })).toBeVisible();
  await upload(page, evilPath, `e2e-evil-path-${run}.tar.gz`);
  await expect(page.getByRole("alert").filter({ hasText: "源码路径不合法: ../evil.txt" })).toBeVisible({ timeout: 15000 });
  // 被拒绝的包没有产生快照区块（换文件时旧快照展示被清除，新快照未生成）。
  await expect(page.getByText("源码快照状态")).toHaveCount(0);

  // 恶意包 2（平台排除的敏感路径 .env）：同样被服务端真实拒绝。
  await upload(page, sensitiveFile, `e2e-sensitive-${run}.tar.gz`);
  await expect(page.getByRole("alert").filter({ hasText: "源码包含禁止上传的敏感文件: .env" })).toBeVisible({ timeout: 15000 });
  await expect(page.getByText("源码快照状态")).toHaveCount(0);

  // 恶意包不落 READY 的服务端证据：合法快照仍可按 ID 查询且状态仍为 READY，
  // 当前实例 UI 不提供快照列表，失败上传不暴露快照 ID，故以查询复核间接验证。
  await page.getByLabel("快照 ID").fill(snapshotId);
  await page.getByRole("button", { name: "查询快照" }).click();
  await expect(page.getByText(`快照：`)).toBeVisible();
  await expect(page.getByText("状态：READY · Dockerfile：Dockerfile")).toBeVisible();

  // 刷新复核：合法快照仍按 ID 查询为 READY（真实持久化，未受恶意上传影响）。
  await page.reload();
  await page.getByLabel("快照 ID").fill(snapshotId);
  await page.getByRole("button", { name: "查询快照" }).click();
  await expect(page.getByText("状态：READY · Dockerfile：Dockerfile")).toBeVisible({ timeout: 10000 });

  // 纯源码包（不指定 Dockerfile）：同样得到 READY 快照，页面说明构建时自动选择构建方式。
  await page.getByRole("link", { name: "源码", exact: true }).click();
  await expect(page.getByRole("heading", { name: "源码上传与校验" })).toBeVisible();
  await upload(page, sourceOnly, `e2e-source-only-${run}.tar.gz`);
  await expect(page.getByText("源码快照状态：READY")).toBeVisible({ timeout: 15000 });
  await expect(metaValue(page, "Dockerfile")).toContainText("未指定，创建构建时自动选择");
  await expect(page.getByRole("row", { name: /package\.json/ })).toBeVisible();
});
