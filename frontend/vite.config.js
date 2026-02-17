import { defineConfig, loadEnv } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const apiTarget = env.VITE_API_TARGET || "http://127.0.0.1:8080";
  const uiBase = env.VITE_BASE || (mode === "production" ? "/ui/" : "/");

  return {
    base: uiBase,
    plugins: [vue()],
    build: {
      rollupOptions: {
        output: {
          manualChunks(id) {
            const normalized = id.replace(/\\/g, "/");
            if (!normalized.includes("/node_modules/")) {
              return undefined;
            }
            if (
              normalized.includes("/node_modules/vue/") ||
              normalized.includes("/node_modules/@vue/")
            ) {
              return "vendor-vue";
            }
            if (
              normalized.includes("/node_modules/element-plus/es/components/")
            ) {
              const marker = "/node_modules/element-plus/es/components/";
              const componentPath = normalized.split(marker)[1] || "";
              const componentName = componentPath.split("/")[0];
              const ch = componentName[0]?.toLowerCase() || "z";
              return ch <= "m" ? "vendor-element-c1" : "vendor-element-c2";
            }
            if (
              normalized.includes("/node_modules/element-plus/es/locale/")
            ) {
              return "vendor-element-locale";
            }
            if (
              normalized.includes("/node_modules/element-plus/") ||
              normalized.includes("/node_modules/@element-plus/")
            ) {
              return undefined;
            }
            if (normalized.includes("/node_modules/@fortawesome/")) {
              return "vendor-icons";
            }
            return undefined;
          },
        },
      },
    },
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
