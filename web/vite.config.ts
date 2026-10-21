import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": process.env.VITE_CONTROL_PROXY ?? "http://127.0.0.1:8080",
    },
  },
  test: {
    environment: "jsdom",
  },
});
