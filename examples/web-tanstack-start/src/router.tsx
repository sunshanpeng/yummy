import { createRouter as createTanStackRouter } from "@tanstack/react-router";
import { routeTree } from "./routeTree.gen";

// 构建入口按约定导入 named export createRouter（TanStack Start 1.130）。
export function createRouter() {
  return createTanStackRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });
}

export function getRouter() {
  return createRouter();
}
