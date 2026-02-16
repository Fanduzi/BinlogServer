import { defineConfig, loadEnv } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const apiTarget = env.VITE_API_TARGET || "http://127.0.0.1:8080";
  const uiBase = env.VITE_BASE || (mode === "production" ? "/ui/" : "/");

  return {
    base: uiBase,
    plugins: [vue()],
    server: {
      port: 5173,
      proxy: {
        "/api": {
          target: apiTarget,
          changeOrigin: true,
        },
        "/healthz": {
          target: apiTarget,
          changeOrigin: true,
        },
      },
    },
  };
});
