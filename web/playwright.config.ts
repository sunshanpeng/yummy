import { defineConfig } from "@playwright/test";

// 只连接父级已经启动的真实服务，不自动创建开发服务器，也不保存凭证截图或 trace。
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  use: { baseURL: process.env.YUMMY_E2E_BASE_URL ?? "http://127.0.0.1:8080", screenshot: "off", video: "off", trace: "off" },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
