/// <reference types="vitest/config" />
import path from 'node:path'
import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'

// The Console is embedded at /ui/ (scripts/build-ui.sh builds with VITE_BASE=/ui/
// into internal/ui/static). Develop against a running binlog-server via the /api proxy.
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const apiTarget = env.VITE_API_TARGET || 'http://127.0.0.1:8080'
  return {
    base: env.VITE_BASE || '/',
    plugins: [
      tanstackRouter({ target: 'react', autoCodeSplitting: true }),
      react(),
      tailwindcss(),
    ],
    build: {
      rollupOptions: {
        output: {
          manualChunks(id) {
            if (!id.includes('/node_modules/')) return undefined
            if (/\/node_modules\/(react|react-dom|scheduler)\//.test(id)) return 'vendor-react'
            if (id.includes('/node_modules/@tanstack/')) return 'vendor-router'
            if (/\/node_modules\/(radix-ui|@radix-ui)\//.test(id)) return 'vendor-radix'
            return undefined
          },
        },
      },
    },
    test: { environment: 'node', include: ['src/**/*.test.ts'] },
    resolve: { alias: { '@': path.resolve(__dirname, './src') } },
    server: {
      port: 5174,
      proxy: {
        '/api': { target: apiTarget, changeOrigin: true },
        '/healthz': { target: apiTarget, changeOrigin: true },
        // Auth discovery (GET /auth/config, anonymous).
        '/auth': { target: apiTarget, changeOrigin: true },
        // The classic console, for side-by-side checks.
        '/ui-classic': { target: apiTarget, changeOrigin: true },
      },
    },
  }
})
