import { createRootRoute, HeadContent, Outlet } from "@tanstack/react-router";

export const Route = createRootRoute({
  head: () => ({ meta: [{ title: "Yummy · TanStack Start 示例" }] }),
  component: () => (
    <html lang="zh-CN">
      <head><HeadContent /></head>
      <body style={{ fontFamily: "system-ui", padding: 40 }}>
        <Outlet />
      </body>
    </html>
  ),
});
