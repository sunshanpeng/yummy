// 平台健康探针路径：/healthz（对应 Node.js 服务模板）。
import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/healthz")({
  server: {
    handlers: {
      GET: () => Response.json({ status: "ok" }),
    },
  },
});
