import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": { target: process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/auth": { target: process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/oauth": { target: process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/.well-known": { target: process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/mcp": { target: process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080", changeOrigin: true },
      "/transfer": {
        target: process.env.VITE_TRANSFER_PROXY ?? "http://127.0.0.1:8081", changeOrigin: false,
        headers: { host: new URL(process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080").host },
      },
    },
  },
  build: {
    rollupOptions: { input: { main: "index.html", demo: "demo.html" } },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
