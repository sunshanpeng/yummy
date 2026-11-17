import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/")({
  component: () => (
    <main>
      <h1>Hello from Yummy PaaS</h1>
      <p>TanStack Start SSR 示例。健康检查：<a href="/healthz">/healthz</a></p>
    </main>
  ),
});
